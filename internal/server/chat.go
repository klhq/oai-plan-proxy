package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/klhq/oai-plan-proxy/internal/chat"
	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// chatHandler serves a Chat Completions request through the plan's
// Responses route.
func (p *proxy) chatHandler(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	tr, err := chat.Translate(raw)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if p.opts.Strict && len(tr.Dropped) > 0 {
		writeParamError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_parameter", tr.Dropped[0],
			tr.Dropped[0]+" has no equivalent with ChatGPT plan usage; remove it, or run serve without --strict to drop it")
		return
	}
	req, err := responses.Sanitize(tr.Body, p.opts.Strict)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	dropped := slices.Concat(tr.Dropped, req.Dropped)
	slices.Sort(dropped)
	resp, ok := p.sendResponses(w, r, req.Body, dropped)
	if !ok {
		return
	}
	defer resp.Body.Close()
	if tr.Stream {
		streamChat(w, resp.Body, chat.NewStreamer(tr.IncludeUsage))
		return
	}
	writeCompleted(w, resp.Body, chat.Completion)
}

// streamChat answers a streaming caller with chat.completion.chunk events.
// Nothing is written until the first chunk, so a failure before it can
// still become a real HTTP error.
func streamChat(w http.ResponseWriter, body io.Reader, s *chat.Streamer) {
	events := responses.NewEventReader(body)
	flusher, _ := w.(http.Flusher)
	started := false
	for {
		ev, err := events.Next()
		if err != nil {
			if !started {
				writeError(w, http.StatusBadGateway, "server_error", "upstream_incomplete",
					"the upstream stream ended before any output")
			}
			return
		}
		if f, ok := failure(ev); ok {
			if !started {
				writeFailure(w, f)
				return
			}
			writeStreamError(w, f)
			return
		}
		chunks := s.Chunks(ev)
		if len(chunks) > 0 && !started {
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
		}
		for _, c := range chunks {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", c); err != nil {
				return
			}
		}
		if ev.Type == "response.completed" {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
		if flusher != nil && started {
			flusher.Flush()
		}
		if ev.Type == "response.completed" {
			return
		}
	}
}

// writeStreamError reports a failure after output has started, as one data
// line with an OpenAI error object. The stream then ends without [DONE], so
// the caller cannot mistake it for a complete answer.
func writeStreamError(w http.ResponseWriter, f upstreamFailure) {
	typ := "server_error"
	if f.Code == usageLimitCode {
		typ = "insufficient_quota"
	}
	d := apiErrorDetail{Message: f.Message, Type: typ}
	if f.Code != "" {
		d.Code = &f.Code
	}
	b, _ := json.Marshal(apiError{Error: d})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}
