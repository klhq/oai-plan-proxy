package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// requestNote holds what handlers learn about a request for its log record.
type requestNote struct {
	model   string
	stream  *bool
	dropped []string
	usage   *tokenUsage
	// plan holds the plan usage windows upstream reported.
	plan []usageWindow
	// upstreamStatus is the HTTP status of an upstream error answer.
	upstreamStatus int
	errorCode      string
	// failed means upstream reported a failure inside the event stream.
	failed bool
	// usageLimit means the caller got the usage-limit 429.
	usageLimit bool
	// retryAfter is the Retry-After the caller got, in seconds.
	retryAfter int
}

// tokenUsage is the usage object of a completed response.
type tokenUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// observe records what an upstream event tells about the request.
func (n *requestNote) observe(ev responses.Event) {
	if f, ok := failure(ev); ok {
		n.failed = true
		n.errorCode = f.logCode()
		return
	}
	if ev.Type != "response.completed" {
		return
	}
	var done struct {
		Response struct {
			Usage *tokenUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(ev.Data, &done) == nil && done.Response.Usage != nil {
		n.usage = done.Response.Usage
	}
}

// attrs returns the note's log attributes, leaving out unset ones.
func (n *requestNote) attrs() []any {
	var a []any
	if n.model != "" {
		a = append(a, "model", n.model)
	}
	if n.stream != nil {
		a = append(a, "stream", *n.stream)
	}
	if len(n.dropped) > 0 {
		a = append(a, "dropped", n.dropped)
	}
	if n.upstreamStatus != 0 {
		a = append(a, "upstream_status", n.upstreamStatus)
	}
	if n.errorCode != "" {
		a = append(a, "error_code", n.errorCode)
	}
	if n.usage != nil {
		a = append(a,
			"input_tokens", n.usage.InputTokens,
			"output_tokens", n.usage.OutputTokens,
			"cached_tokens", n.usage.InputTokensDetails.CachedTokens)
	}
	if n.retryAfter > 0 {
		a = append(a, "retry_after_s", n.retryAfter)
	}
	for _, w := range n.plan {
		a = append(a, w.attrs()...)
	}
	return a
}

// statusWriter records the status sent to the caller and carries the
// request's log note. It keeps Flush, so streams still reach the caller one
// event at a time.
type statusWriter struct {
	http.ResponseWriter
	status int
	note   requestNote
}

// noteOf returns the log note for the request w answers. A writer without
// one, as in a handler called directly, gets a note nobody reads.
func noteOf(w http.ResponseWriter) *requestNote {
	if sw, ok := w.(*statusWriter); ok {
		return &sw.note
	}
	return &requestNote{}
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests writes one record per request after the response ends.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		// The container healthcheck probes every 30 seconds; only a failed
		// probe is worth a line.
		if r.URL.Path == "/healthz" && status == http.StatusOK {
			return
		}
		// An upstream failure noted while the caller got 200 arrived after
		// output had started, inside the stream.
		streamError := status == http.StatusOK && sw.note.failed
		level := slog.LevelInfo
		if status >= http.StatusInternalServerError || sw.note.usageLimit || streamError {
			level = slog.LevelWarn
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		attrs = append(attrs, sw.note.attrs()...)
		if streamError {
			attrs = append(attrs, "stream_error", true)
		}
		if r.Context().Err() != nil {
			attrs = append(attrs, "canceled", true)
		}
		logger.Log(context.Background(), level, "request", attrs...)
	})
}
