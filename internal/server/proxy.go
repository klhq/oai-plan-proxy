package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/tokens"
)

type proxy struct {
	opts Options
	// slots holds one token per model request in flight. It is nil when
	// there is no limit.
	slots chan struct{}
}

// copyResponse copies an upstream answer's status, Content-Type and body.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// send makes an upstream request with the OAuth access token. A non-nil
// body is sent as JSON. After an upstream 401 it refreshes the rejected
// token and retries once.
func (p *proxy) send(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	tok, err := p.opts.Tokens.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := p.do(ctx, method, path, body, tok)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	resp.Body.Close()
	tok, err = p.opts.Tokens.RefreshRejected(ctx, tok)
	if err != nil {
		return nil, err
	}
	return p.do(ctx, method, path, body, tok)
}

func (p *proxy) do(ctx context.Context, method, path string, body []byte, tok string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.opts.Upstream+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return p.opts.Client.Do(req)
}

// v1 guards an API route. It checks the incoming API key first, so an
// unauthenticated caller learns nothing about the sign-in, then answers 503
// while no usable sign-in exists.
func (p *proxy) v1(next http.HandlerFunc) http.Handler {
	return p.keyed(func(w http.ResponseWriter, r *http.Request) {
		if state := p.opts.Login.State(); state != auth.StateValid {
			writeNoLogin(w, state)
			return
		}
		next(w, r)
	})
}

func writeNoLogin(w http.ResponseWriter, state auth.State) {
	writeError(w, http.StatusServiceUnavailable, "server_error", "login_"+string(state),
		"the proxy has no usable ChatGPT sign-in ("+string(state)+"); run `oai-plan-proxy login` on the proxy host")
}

// keyed rejects requests without the configured API key.
func (p *proxy) keyed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.authorized(r) {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
				"missing or incorrect API key; send it as `Authorization: Bearer <key>`")
			return
		}
		next(w, r)
	})
}

// notFound answers API routes outside ChatGPT plan usage.
func notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "invalid_request_error", "unknown_url",
		r.Method+" "+r.URL.Path+" is not served: ChatGPT plan usage covers only /v1/models, /v1/responses, and /v1/chat/completions")
}

// authorized reports whether r carries the configured API key. With no key
// configured every request is authorized.
func (p *proxy) authorized(r *http.Request) bool {
	if p.opts.APIKey == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(p.opts.APIKey)) == 1
}

// writeSendError answers a request that never got an upstream response.
func writeSendError(w http.ResponseWriter, err error) {
	if errors.Is(err, tokens.ErrSignInDead) {
		writeNoLogin(w, auth.StateDead)
		return
	}
	writeError(w, http.StatusBadGateway, "server_error", "upstream_unavailable", err.Error())
}
