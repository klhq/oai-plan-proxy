package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// maxRequestBody bounds a /v1/responses body. Inline images make bodies
// large, so the bound is generous.
const maxRequestBody = 32 << 20

// droppedHeader lists the parameters removed from a request.
const droppedHeader = "x-oai-plan-proxy-dropped-params"

// responsesHandler sanitizes a Responses request and forwards it.
func (p *proxy) responsesHandler(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	req, err := responses.Sanitize(raw, p.opts.Strict)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	noteOf(w).stream = &req.Stream
	resp, ok := p.sendResponses(w, r, req.Body, req.Dropped)
	if !ok {
		return
	}
	defer resp.Body.Close()
	if req.Stream {
		streamEvents(w, resp.Body)
		return
	}
	writeCompleted(w, resp.Body, func(response []byte) ([]byte, error) { return response, nil })
}

// readBody reads a bounded request body. When it returns false it has
// already answered w.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", err.Error())
		return nil, false
	}
	return raw, true
}

// writeRequestError answers a request the proxy will not forward.
func writeRequestError(w http.ResponseWriter, err error) {
	var reqErr *responses.Error
	if errors.As(err, &reqErr) {
		writeParamError(w, http.StatusBadRequest, "invalid_request_error", reqErr.Code, reqErr.Param, reqErr.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "server_error", "", err.Error())
}

// sendResponses names the dropped parameters and posts body to the
// upstream Responses route. It returns a 2xx answer, or false after it has
// answered w with the failure.
func (p *proxy) sendResponses(w http.ResponseWriter, r *http.Request, body []byte, dropped []string) (*http.Response, bool) {
	if len(dropped) > 0 {
		w.Header().Set(droppedHeader, strings.Join(dropped, ","))
	}
	var sent struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &sent)
	note := noteOf(w)
	note.model = sent.Model
	note.dropped = dropped
	resp, err := p.send(r.Context(), http.MethodPost, "/responses", body)
	if err != nil {
		writeSendError(w, err)
		return nil, false
	}
	note.plan = usageWindows(resp.Header)
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		writeUpstreamError(w, resp)
		return nil, false
	}
	return resp, true
}

// writeCompleted answers a non-streaming caller with the response object
// from the upstream response.completed event, rewritten by render.
func writeCompleted(w http.ResponseWriter, body io.Reader, render func(response []byte) ([]byte, error)) {
	events := responses.NewEventReader(body)
	note := noteOf(w)
	for {
		ev, err := events.Next()
		if err != nil {
			writeError(w, http.StatusBadGateway, "server_error", "upstream_incomplete",
				"the upstream stream ended before response.completed")
			return
		}
		note.observe(ev)
		if f, ok := failure(ev); ok {
			writeFailure(w, f)
			return
		}
		if ev.Type != "response.completed" {
			continue
		}
		var done struct {
			Response json.RawMessage `json:"response"`
		}
		_ = json.Unmarshal(ev.Data, &done)
		out, err := render(done.Response)
		if err != nil {
			writeError(w, http.StatusBadGateway, "server_error", "upstream_invalid",
				"the upstream response could not be read: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}
}

// maxErrorBody bounds how much of an upstream error answer is read.
const maxErrorBody = 1 << 20

// usageLimitCode is the error upstream sends when the plan's shared usage is
// spent. Retrying cannot help until the limit resets.
const usageLimitCode = "subscription_sharing_usage_limit_exceeded"

// writeUpstreamError passes an upstream HTTP error through, except the usage
// limit, which becomes a 429 that tells the caller not to retry.
func writeUpstreamError(w http.ResponseWriter, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var e struct {
		Error upstreamFailure `json:"error"`
	}
	parsed := json.Unmarshal(body, &e) == nil
	note := noteOf(w)
	note.upstreamStatus = resp.StatusCode
	note.errorCode = e.Error.logCode()
	if parsed && e.Error.usageLimit() {
		writeUsageLimit(w, e.Error)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// writeUsageLimit answers 429 with x-should-retry: false, so a gateway such
// as Bifrost falls back to its next provider instead of retrying.
func writeUsageLimit(w http.ResponseWriter, f upstreamFailure) {
	message := f.Message
	if message == "" {
		message = "the ChatGPT plan usage limit is exhausted"
	}
	note := noteOf(w)
	note.usageLimit = true
	if wait := retryAfter(f, note.plan, time.Now()); wait > 0 {
		note.retryAfter = wait
		w.Header().Set("Retry-After", strconv.Itoa(wait))
	}
	w.Header().Set("x-should-retry", "false")
	writeError(w, http.StatusTooManyRequests, "insufficient_quota", usageLimitCode, message)
}

// preOutput are the events upstream sends before any output. A stream is
// held back until the first other event, so a failure up to that point can
// still become a real HTTP error.
var preOutput = map[string]bool{
	"response.created":     true,
	"response.in_progress": true,
	"response.queued":      true,
}

// streamEvents forwards upstream events to a streaming caller, flushing
// after each one.
func streamEvents(w http.ResponseWriter, body io.Reader) {
	events := responses.NewEventReader(body)
	note := noteOf(w)
	var held [][]byte
	for {
		ev, err := events.Next()
		if err != nil {
			writeError(w, http.StatusBadGateway, "server_error", "upstream_incomplete",
				"the upstream stream ended before any output")
			return
		}
		note.observe(ev)
		if f, ok := failure(ev); ok {
			writeFailure(w, f)
			return
		}
		held = append(held, ev.Raw)
		if !preOutput[ev.Type] {
			break
		}
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for _, raw := range held {
		if _, err := w.Write(raw); err != nil {
			return
		}
	}
	// After the first output event the stream passes through as upstream
	// sent it, failures included.
	for {
		if flusher != nil {
			flusher.Flush()
		}
		ev, err := events.Next()
		if err != nil {
			return
		}
		note.observe(ev)
		if _, err := w.Write(ev.Raw); err != nil {
			return
		}
	}
}

// upstreamFailure is an error upstream reported inside the event stream.
type upstreamFailure struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Message string `json:"message"`
	// ResetsAt is when a usage limit resets, in Unix seconds.
	ResetsAt int64 `json:"resets_at"`
}

// codexUsageLimitType is the error type Codex reads as the usage limit.
const codexUsageLimitType = "usage_limit_reached"

// usageLimit reports whether f says the plan's usage is spent, in either
// form upstream uses.
func (f upstreamFailure) usageLimit() bool {
	return f.Code == usageLimitCode || f.Type == codexUsageLimitType
}

// logCode names f in the request record: its code, or its type when the
// code is empty.
func (f upstreamFailure) logCode() string {
	if f.Code != "" {
		return f.Code
	}
	return f.Type
}

// failure reports whether ev is a failed response or an error event and
// extracts its error. A failed response nests the error in response.error.
// An error event carries code and message at the top level, or nests them
// in error the way Codex reads them.
func failure(ev responses.Event) (upstreamFailure, bool) {
	switch ev.Type {
	case "response.failed":
		var failed struct {
			Response struct {
				Error upstreamFailure `json:"error"`
			} `json:"response"`
		}
		_ = json.Unmarshal(ev.Data, &failed)
		return failed.Response.Error, true
	case "error":
		var e struct {
			Code    string           `json:"code"`
			Message string           `json:"message"`
			Error   *upstreamFailure `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		if e.Error != nil {
			return *e.Error, true
		}
		return upstreamFailure{Code: e.Code, Message: e.Message}, true
	}
	return upstreamFailure{}, false
}

// writeFailure answers with an HTTP error for a failure upstream reported
// before any output.
func writeFailure(w http.ResponseWriter, f upstreamFailure) {
	if f.usageLimit() {
		writeUsageLimit(w, f)
		return
	}
	writeError(w, http.StatusBadGateway, "server_error", f.Code, f.Message)
}
