// Package server serves the proxy's HTTP routes.
package server

import (
	"encoding/json"
	"net/http"

	"github.com/klhq/oai-plan-proxy/internal/auth"
)

// LoginReporter reports the current sign-in state.
type LoginReporter interface {
	State() auth.State
}

// Options configures the HTTP handler.
type Options struct {
	Login LoginReporter
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
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
