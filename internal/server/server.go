// Package server serves the proxy's HTTP routes.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
)

// LoginReporter reports the current sign-in state.
type LoginReporter interface {
	State() auth.State
}

// TokenSource hands out OAuth access tokens for upstream requests.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
	// RefreshRejected handles an upstream 401 for the token rejected and
	// returns the token to retry with.
	RefreshRejected(ctx context.Context, rejected string) (string, error)
}

// Options configures the HTTP handler.
type Options struct {
	Login  LoginReporter
	Tokens TokenSource
	// Upstream is the API base URL, such as https://api.openai.com/v1.
	Upstream string
	// Client sends upstream requests. It must not set an overall timeout,
	// because response streams run for minutes.
	Client *http.Client
	// APIKey, when set, is the bearer value every /v1 request must carry.
	APIKey string
	// Strict rejects preview-limited parameters with 400 instead of
	// dropping them.
	Strict bool
	// Logger receives one record per request. Nil discards them.
	Logger *slog.Logger
	// MaxConcurrent, when above 0, limits the model requests in flight.
	MaxConcurrent int
	// UsageLimitPause, when above 0, makes the proxy answer model requests
	// with the usage-limit 429 at once, without asking upstream, after
	// upstream reports the plan's usage spent. The pause lasts until the
	// reset time upstream gives, or this long when it gives none.
	UsageLimitPause time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// New returns the proxy's HTTP handler.
func New(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		state := opts.Login.State()
		code, status := http.StatusServiceUnavailable, "error"
		if state == auth.StateValid {
			code, status = http.StatusOK, "ok"
		}
		writeJSON(w, code, map[string]string{
			"status": status,
			"login":  string(state),
		})
	})
	p := &proxy{opts: opts}
	if opts.MaxConcurrent > 0 {
		p.slots = make(chan struct{}, opts.MaxConcurrent)
	}
	mux.Handle("GET /v1/models", p.v1(p.listModels))
	mux.Handle("GET /v1/models/{id}", p.v1(p.getModel))
	mux.Handle("POST /v1/responses", p.v1(p.paused(p.limited(p.responsesHandler))))
	mux.Handle("POST /v1/chat/completions", p.v1(p.paused(p.limited(p.chatHandler))))
	mux.Handle("/v1/", p.keyed(notFound))
	return logRequests(opts.Logger, mux)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
