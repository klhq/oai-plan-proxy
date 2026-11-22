package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// statusWriter records the status sent to the caller. It keeps Flush, so
// streams still reach the caller one event at a time.
type statusWriter struct {
	http.ResponseWriter
	status int
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
		level := slog.LevelInfo
		if status >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if r.Context().Err() != nil {
			attrs = append(attrs, "canceled", true)
		}
		logger.Log(context.Background(), level, "request", attrs...)
	})
}
