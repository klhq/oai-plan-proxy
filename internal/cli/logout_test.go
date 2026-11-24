package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/cli"
	"github.com/klhq/oai-plan-proxy/internal/oauth"
	"github.com/klhq/oai-plan-proxy/internal/store"
)

// fakeRevocation serves a discovery document and a revocation endpoint that
// answers status. Each revocation form is sent on the returned channel.
func (h *harness) fakeRevocation(t *testing.T, status int) <-chan url.Values {
	t.Helper()
	forms := make(chan url.Values, 4)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"revocation_endpoint": srv.URL + "/oauth/revoke"})
	})
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		forms <- r.PostForm
		w.WriteHeader(status)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h.client = srv.Client()
	h.oauth = oauth.Endpoints{
		AuthorizeURL: "https://auth.example.test/api/accounts/authorize",
		TokenURL:     srv.URL + "/api/accounts/oauth/token",
		Issuer:       "https://auth.example.test",
		Resource:     "https://api.example.test/v1",
		DiscoveryURL: srv.URL + "/.well-known/openid-configuration",
	}
	return forms
}

func (h *harness) store(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(h.vars["OAI_PLAN_PROXY_DATA_DIR"])
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// hostID returns h's host ID, creating it on first use.
func (h *harness) hostID(t *testing.T) string {
	t.Helper()
	id, err := h.store(t).HostID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func receiveForm(t *testing.T, forms <-chan url.Values) url.Values {
	t.Helper()
	select {
	case f := <-forms:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("the revocation endpoint received no request")
		return nil
	}
}

func TestLogoutRevokesTheRefreshTokenAndKeepsTheHostID(t *testing.T) {
	h := newHarness(t)
	forms := h.fakeRevocation(t, http.StatusOK)
	h.saveCredentials(t, signedIn())
	hostID, _ := h.store(t).HostID()

	code := cli.Run(context.Background(), []string{"logout"}, h.env())

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, h.stderr.String())
	}
	form := receiveForm(t, forms)
	if form.Get("token") != "secret-refresh-token" || form.Get("client_id") != "oaiapp_test" {
		t.Fatalf("revocation form = %v, want the stored refresh token and client ID", form)
	}
	if _, err := h.store(t).Load(); !errors.Is(err, store.ErrNoCredentials) {
		t.Fatalf("credentials after logout: %v, want none", err)
	}
	if got, _ := h.store(t).HostID(); got != hostID {
		t.Fatalf("host id = %q after logout, want %q", got, hostID)
	}
}

func TestLogoutKeepsTheCredentialsWhenRevocationFails(t *testing.T) {
	h := newHarness(t)
	forms := h.fakeRevocation(t, http.StatusServiceUnavailable)
	h.saveCredentials(t, signedIn())

	code := cli.Run(context.Background(), []string{"logout"}, h.env())

	receiveForm(t, forms)
	if code == 0 {
		t.Fatal("logout exited 0 although the revocation failed")
	}
	if c, err := h.store(t).Load(); err != nil || c.RefreshToken != "secret-refresh-token" {
		t.Fatalf("credentials after a failed logout = %+v, %v; want them kept so logout can be retried", c, err)
	}
}

func TestLogoutOfADeadSignInDeletesItWithoutRevoking(t *testing.T) {
	h := newHarness(t)
	forms := h.fakeRevocation(t, http.StatusBadRequest)
	h.saveCredentials(t, store.Credentials{ClientID: "oaiapp_test"})

	code := cli.Run(context.Background(), []string{"logout"}, h.env())

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, h.stderr.String())
	}
	select {
	case f := <-forms:
		t.Fatalf("logout revoked %v although no refresh token was stored", f)
	default:
	}
	if _, err := h.store(t).Load(); !errors.Is(err, store.ErrNoCredentials) {
		t.Fatalf("credentials after logout: %v, want none", err)
	}
}
