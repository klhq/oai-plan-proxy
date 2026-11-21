package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/oauth"
)

var testEndpoints = oauth.Endpoints{
	AuthorizeURL: "https://auth.example.test/api/accounts/authorize",
	TokenURL:     "https://auth.example.test/api/accounts/oauth/token",
	Issuer:       "https://auth.example.test",
	Resource:     "https://api.example.test/v1",
}

// fakeTokenServer counts token requests and answers each with handler.
type fakeTokenServer struct {
	*httptest.Server
	requests atomic.Int32
}

func newFakeTokenServer(t *testing.T, handler http.HandlerFunc) *fakeTokenServer {
	t.Helper()
	f := &fakeTokenServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTokenServer) endpoints() oauth.Endpoints {
	ep := testEndpoints
	ep.TokenURL = f.URL + "/api/accounts/oauth/token"
	return ep
}

var fixedNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// unsignedJWT builds a JWT with the given claims and a dummy signature. The
// proxy checks claims only, because it receives the id_token directly from
// the token endpoint over TLS.
func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		enc.EncodeToString(payload) + "." + enc.EncodeToString([]byte("sig"))
}

func idClaims(flow *oauth.Flow, t *testing.T) map[string]any {
	return map[string]any{
		"iss":   testEndpoints.Issuer,
		"aud":   "oaiapp_issued",
		"sub":   "user-sub-1",
		"email": "user@example.com",
		"nonce": query(t, flow.URL).Get("nonce"),
		"exp":   fixedNow.Add(time.Hour).Unix(),
	}
}

func goodCallback(t *testing.T, flow *oauth.Flow) url.Values {
	return url.Values{
		"code":      {"code-1"},
		"state":     {query(t, flow.URL).Get("state")},
		"client_id": {"oaiapp_issued"},
	}
}

func newFlow(t *testing.T, ep oauth.Endpoints, p oauth.Params) *oauth.Flow {
	t.Helper()
	p.Now = func() time.Time { return fixedNow }
	flow, err := oauth.NewFlow(ep, p)
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

func writeTokens(w http.ResponseWriter, idToken string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "access-1",
		"refresh_token": "refresh-1",
		"id_token":      idToken,
		"token_type":    "Bearer",
		"expires_in":    3600,
		"scope":         oauth.Scope,
	})
}

func TestValidCallbackExchangesTheCodeWithTheIssuedClientID(t *testing.T) {
	var (
		flow *oauth.Flow
		form url.Values
	)
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form = r.PostForm
		writeTokens(w, unsignedJWT(t, idClaims(flow, t)))
	})
	flow = newFlow(t, tokens.endpoints(), firstRegistration())

	got, err := flow.Exchange(context.Background(), goodCallback(t, flow), tokens.Client())
	if err != nil {
		t.Fatal(err)
	}

	wantForm := map[string]string{
		"grant_type":   "authorization_code",
		"client_id":    "oaiapp_issued",
		"code":         "code-1",
		"redirect_uri": "http://127.0.0.1:1455/auth/callback",
		"resource":     testEndpoints.Resource,
	}
	for k, v := range wantForm {
		if form.Get(k) != v {
			t.Errorf("token form %s = %q, want %q", k, form.Get(k), v)
		}
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if challenge := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != query(t, flow.URL).Get("code_challenge") {
		t.Errorf("code_verifier does not match the code_challenge in the authorization URL")
	}
	want := oauth.Result{
		ClientID:     "oaiapp_issued",
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		ExpiresAt:    fixedNow.Add(time.Hour),
		Subject:      "user-sub-1",
		Email:        "user@example.com",
	}
	got.IDToken = ""
	if got != want {
		t.Fatalf("result = %+v, want %+v", got, want)
	}
}

func TestFirstRegistrationCallbackWithoutAnIssuedClientIDIsIncomplete(t *testing.T) {
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {})
	flow := newFlow(t, tokens.endpoints(), firstRegistration())
	callback := goodCallback(t, flow)
	callback.Del("client_id")

	_, err := flow.Exchange(context.Background(), callback, tokens.Client())

	if err == nil || !strings.Contains(err.Error(), "client") {
		t.Fatalf("err = %v, want an incomplete-registration error", err)
	}
	if n := tokens.requests.Load(); n != 0 {
		t.Fatalf("token endpoint got %d requests, want 0", n)
	}
}

func TestDeniedConsentStopsTheSignInWithTheOAuthError(t *testing.T) {
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {})
	flow := newFlow(t, tokens.endpoints(), firstRegistration())
	callback := url.Values{"error": {"access_denied"}, "state": {query(t, flow.URL).Get("state")}}

	_, err := flow.Exchange(context.Background(), callback, tokens.Client())

	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v, want it to name access_denied", err)
	}
	if n := tokens.requests.Load(); n != 0 {
		t.Fatalf("token endpoint got %d requests, want 0", n)
	}
}

func TestTokenEndpointErrorsCarryTheirCode(t *testing.T) {
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"code expired"}`))
	})
	flow := newFlow(t, tokens.endpoints(), firstRegistration())

	_, err := flow.Exchange(context.Background(), goodCallback(t, flow), tokens.Client())

	var tokenErr *oauth.TokenError
	if !errors.As(err, &tokenErr) {
		t.Fatalf("err = %v, want a *oauth.TokenError", err)
	}
	if tokenErr.Code != "invalid_grant" || tokenErr.Status != http.StatusBadRequest {
		t.Fatalf("TokenError = %+v, want invalid_grant with status 400", tokenErr)
	}
}

func TestIDTokenClaimsAreValidated(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"nonce from another attempt", func(c map[string]any) { c["nonce"] = "other-nonce" }},
		{"audience is another client", func(c map[string]any) { c["aud"] = "oaiapp_other" }},
		{"issuer is another server", func(c map[string]any) { c["iss"] = "https://evil.example.test" }},
		{"token already expired", func(c map[string]any) { c["exp"] = fixedNow.Add(-time.Minute).Unix() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var flow *oauth.Flow
			tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
				claims := idClaims(flow, t)
				tc.mutate(claims)
				writeTokens(w, unsignedJWT(t, claims))
			})
			flow = newFlow(t, tokens.endpoints(), firstRegistration())

			_, err := flow.Exchange(context.Background(), goodCallback(t, flow), tokens.Client())

			if err == nil {
				t.Fatal("Exchange accepted the id_token")
			}
		})
	}
}

func TestIDTokenAudienceMayBeAList(t *testing.T) {
	var flow *oauth.Flow
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		claims := idClaims(flow, t)
		claims["aud"] = []string{"https://api.example.test/v1", "oaiapp_issued"}
		writeTokens(w, unsignedJWT(t, claims))
	})
	flow = newFlow(t, tokens.endpoints(), firstRegistration())

	if _, err := flow.Exchange(context.Background(), goodCallback(t, flow), tokens.Client()); err != nil {
		t.Fatalf("Exchange rejected a list audience that includes the client ID: %v", err)
	}
}

func TestCallbackWithAForeignStateIsRejectedWithoutATokenRequest(t *testing.T) {
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {})
	flow, err := oauth.NewFlow(tokens.endpoints(), firstRegistration())
	if err != nil {
		t.Fatal(err)
	}
	callback := url.Values{"code": {"code-1"}, "state": {"forged"}, "client_id": {"oaiapp_issued"}}

	_, err = flow.Exchange(context.Background(), callback, tokens.Client())

	if err == nil {
		t.Fatal("Exchange accepted a callback with a foreign state")
	}
	if n := tokens.requests.Load(); n != 0 {
		t.Fatalf("token endpoint got %d requests, want 0", n)
	}
}

func firstRegistration() oauth.Params {
	return oauth.Params{HostID: "host-123", AgentName: "oai-plan-proxy", Port: 1455}
}

func query(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestFirstRegistrationURLCarriesEveryRequiredParameter(t *testing.T) {
	flow, err := oauth.NewFlow(testEndpoints, firstRegistration())
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(flow.URL, testEndpoints.AuthorizeURL+"?") {
		t.Fatalf("URL = %q, want it to start with the authorize endpoint", flow.URL)
	}
	q := query(t, flow.URL)
	want := map[string]string{
		"response_type":         "code",
		"client_id":             "dynamic_agent_client",
		"agent_name_hint":       "oai-plan-proxy",
		"ext_agent_host_id":     "host-123",
		"resource":              testEndpoints.Resource,
		"redirect_uri":          "http://127.0.0.1:1455/auth/callback",
		"scope":                 "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
		"code_challenge_method": "S256",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"code_challenge", "state", "nonce"} {
		if len(q.Get(k)) < 32 {
			t.Errorf("%s = %q, want at least 32 random characters", k, q.Get(k))
		}
	}
}

func TestReturningSignInUsesTheIssuedClientIDWithoutANameHint(t *testing.T) {
	p := firstRegistration()
	p.ClientID = "oaiapp_issued"

	flow, err := oauth.NewFlow(testEndpoints, p)
	if err != nil {
		t.Fatal(err)
	}

	q := query(t, flow.URL)
	if got := q.Get("client_id"); got != "oaiapp_issued" {
		t.Errorf("client_id = %q, want oaiapp_issued", got)
	}
	if q.Has("agent_name_hint") {
		t.Errorf("agent_name_hint = %q, want it absent", q.Get("agent_name_hint"))
	}
}
