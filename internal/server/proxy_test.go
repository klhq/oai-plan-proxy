package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/server"
	"github.com/klhq/oai-plan-proxy/internal/tokens"
)

// fakeTokens hands out access tokens. RefreshRejected moves to next.
type fakeTokens struct {
	mu       sync.Mutex
	current  string
	next     string
	rejected []string
	// err, when set, is returned by AccessToken.
	err error
}

func (f *fakeTokens) AccessToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, f.err
}

func (f *fakeTokens) RefreshRejected(_ context.Context, rejected string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, rejected)
	f.current = f.next
	return f.current, nil
}

// upstreamCall is one request the fake upstream received.
type upstreamCall struct {
	method, path, auth string
}

// fakeUpstream records requests and answers each with respond.
type fakeUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []upstreamCall
	respond func(w http.ResponseWriter, r *http.Request)
}

func newUpstream(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{respond: respond}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls = append(u.calls, upstreamCall{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		u.mu.Unlock()
		u.respond(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *fakeUpstream) received() []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.calls...)
}

// proxy builds the handler with a valid login, upstream at u.URL+"/v1".
func proxy(u *fakeUpstream, tok *fakeTokens, apiKey string) http.Handler {
	return server.New(server.Options{
		Login:    fakeLogin{state: auth.StateValid},
		Tokens:   tok,
		Upstream: u.URL + "/v1",
		Client:   u.Client(),
		APIKey:   apiKey,
	})
}

// planModels answers like the plan's /v1/models: a Codex model catalog, not
// an OpenAI model list.
func planModels(body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func TestModelsListsThePlanCatalogInOpenAIShape(t *testing.T) {
	u := newUpstream(t, planModels(`{"models":[{"slug":"gpt-a","display_name":"GPT A","visibility":"list","supported_in_api":true}]}`))
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := get(t, h, "/v1/models")

	want := `{"object":"list","data":[{"id":"gpt-a","object":"model","created":0,"owned_by":"openai"}]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("response = %d %s, want 200 %s", rec.Code, rec.Body.String(), want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	calls := u.received()
	if len(calls) != 1 || calls[0] != (upstreamCall{"GET", "/v1/models", "Bearer access-1"}) {
		t.Fatalf("upstream calls = %+v, want one GET /v1/models with Bearer access-1", calls)
	}
}

func TestModelsLeavesOutModelsThePlanHides(t *testing.T) {
	u := newUpstream(t, planModels(`{"models":[{"slug":"gpt-a","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`))
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := get(t, h, "/v1/models")

	if body := rec.Body.String(); rec.Code != http.StatusOK || strings.Contains(body, "codex-auto-review") || !strings.Contains(body, `"gpt-a"`) {
		t.Fatalf("response = %d %s, want only gpt-a", rec.Code, body)
	}
}

func TestOneModelIsLookedUpInThePlanCatalog(t *testing.T) {
	u := newUpstream(t, planModels(`{"models":[{"slug":"gpt-a","visibility":"list"},{"slug":"gpt-b","visibility":"list"}]}`))
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := get(t, h, "/v1/models/gpt-b")

	want := `{"id":"gpt-b","object":"model","created":0,"owned_by":"openai"}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("response = %d %s, want 200 %s", rec.Code, rec.Body.String(), want)
	}
	if calls := u.received(); len(calls) != 1 || calls[0].path != "/v1/models" {
		t.Fatalf("upstream calls = %+v, want one GET /v1/models: the plan token cannot read /v1/models/{id}", calls)
	}
}

func TestAModelOutsideTheListedCatalogAnswers404ModelNotFound(t *testing.T) {
	u := newUpstream(t, planModels(`{"models":[{"slug":"gpt-a","visibility":"list"},{"slug":"codex-auto-review","visibility":"hide"}]}`))
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	for _, id := range []string{"gpt-z", "codex-auto-review"} {
		rec := get(t, h, "/v1/models/"+id)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", id, rec.Code)
		}
		if _, typ, code := openAIError(t, rec); typ != "invalid_request_error" || code != "model_not_found" {
			t.Fatalf("%s: error = %s/%s, want invalid_request_error/model_not_found", id, typ, code)
		}
	}
}

func TestAnUnreadableCatalogAnswers502InsteadOfAnEmptyList(t *testing.T) {
	u := newUpstream(t, planModels(`<html>maintenance</html>`))
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := get(t, h, "/v1/models")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %s, want 502", rec.Code, rec.Body.String())
	}
	if _, _, code := openAIError(t, rec); code != "upstream_invalid" {
		t.Fatalf("code = %q, want upstream_invalid", code)
	}
}

func TestAnUpstream401IsRetriedOnceWithARefreshedToken(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	})
	tok := &fakeTokens{current: "access-1", next: "access-2"}
	h := proxy(u, tok, "")

	rec := get(t, h, "/v1/models")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the retry", rec.Code)
	}
	if len(tok.rejected) != 1 || tok.rejected[0] != "access-1" {
		t.Fatalf("RefreshRejected calls = %v, want one for access-1", tok.rejected)
	}
	if calls := u.received(); len(calls) != 2 || calls[1].auth != "Bearer access-2" {
		t.Fatalf("upstream calls = %+v, want a retry with access-2", calls)
	}
}

func TestAnUpstreamThatKeepsAnswering401IsRetriedOnlyOnce(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"token rejected"}}`))
	})
	h := proxy(u, &fakeTokens{current: "access-1", next: "access-2"}, "")

	rec := get(t, h, "/v1/models")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 passed through", rec.Code)
	}
	if calls := u.received(); len(calls) != 2 {
		t.Fatalf("upstream received %d requests, want 2 (one retry)", len(calls))
	}
}

// openAIError decodes an OpenAI-shaped error body.
func openAIError(t *testing.T, rec *httptest.ResponseRecorder) (message, typ, code string) {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Message == "" {
		t.Fatalf("body is not an OpenAI-shaped error: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	return body.Error.Message, body.Error.Type, body.Error.Code
}

func TestWithoutAUsableLoginV1AnswersA503ThatPointsToLogin(t *testing.T) {
	for _, state := range []auth.State{auth.StateMissing, auth.StateDead} {
		t.Run(string(state), func(t *testing.T) {
			u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
			h := server.New(server.Options{
				Login:    fakeLogin{state: state},
				Tokens:   &fakeTokens{},
				Upstream: u.URL + "/v1",
				Client:   u.Client(),
			})

			rec := get(t, h, "/v1/models")

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if msg, _, _ := openAIError(t, rec); !strings.Contains(msg, "oai-plan-proxy login") {
				t.Fatalf("message = %q, want it to tell the operator to run oai-plan-proxy login", msg)
			}
			if calls := u.received(); len(calls) != 0 {
				t.Fatalf("upstream received %+v, want no request without a login", calls)
			}
		})
	}
}

func TestAConfiguredAPIKeyGuardsV1ButNotHealthz(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	})
	h := proxy(u, &fakeTokens{current: "access-1"}, "proxy-key")
	request := func(path, authorization string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, authorization := range []string{"", "Bearer wrong-key", "proxy-key"} {
		rec := request("/v1/models", authorization)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status = %d, want 401", authorization, rec.Code)
			continue
		}
		if _, typ, _ := openAIError(t, rec); typ != "invalid_request_error" {
			t.Errorf("Authorization %q: error type = %q, want invalid_request_error", authorization, typ)
		}
	}
	if rec := request("/v1/models", "Bearer proxy-key"); rec.Code != http.StatusOK {
		t.Errorf("matching key: status = %d, want 200", rec.Code)
	}
	if rec := request("/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz without a key: status = %d, want 200", rec.Code)
	}
	for _, c := range u.received() {
		if c.auth != "Bearer access-1" {
			t.Errorf("upstream saw Authorization %q, want only the OAuth token", c.auth)
		}
	}
}

func TestAWrongAPIKeyLearnsNothingAboutTheSignIn(t *testing.T) {
	h := server.New(server.Options{Login: fakeLogin{state: auth.StateDead}, Tokens: &fakeTokens{}, APIKey: "proxy-key"})

	rec := get(t, h, "/v1/models")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 before any sign-in state is revealed", rec.Code)
	}
}

func TestOtherV1PathsAnswerAnOpenAIShaped404(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	for _, path := range []string{"/v1/embeddings", "/v1/images/generations", "/v1/models/a/b"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
			continue
		}
		if msg, _, _ := openAIError(t, rec); !strings.Contains(msg, path) {
			t.Errorf("%s: message = %q, want it to name the path", path, msg)
		}
	}
	if calls := u.received(); len(calls) != 0 {
		t.Fatalf("upstream received %+v, want nothing for unsupported routes", calls)
	}
}

func TestUnknownV1PathsAlsoRequireTheAPIKey(t *testing.T) {
	h := server.New(server.Options{Login: fakeLogin{state: auth.StateValid}, Tokens: &fakeTokens{}, APIKey: "proxy-key"})

	rec := get(t, h, "/v1/embeddings")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 so callers without the key cannot map the routes", rec.Code)
	}
}

func TestATokenFailureAnswersAnOpenAIShaped502(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	h := proxy(u, &fakeTokens{err: errors.New("refresh access token: connection refused")}, "")

	rec := get(t, h, "/v1/models")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	openAIError(t, rec)
}

func TestASignInThatDiesDuringTheRequestAnswers503WithTheLoginHint(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	err := fmt.Errorf("%w (invalid_grant)", tokens.ErrSignInDead)
	h := proxy(u, &fakeTokens{err: err}, "")

	rec := get(t, h, "/v1/models")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if msg, _, _ := openAIError(t, rec); !strings.Contains(msg, "oai-plan-proxy login") {
		t.Fatalf("message = %q, want the login hint", msg)
	}
}
