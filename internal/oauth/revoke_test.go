package oauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/oauth"
)

// revocationServer serves an OpenID discovery document and a revocation
// endpoint. revoke answers the revocation POST after its form is recorded.
type revocationServer struct {
	*httptest.Server
	forms  chan url.Values
	revoke func(w http.ResponseWriter)
}

func newRevocationServer(t *testing.T) *revocationServer {
	t.Helper()
	s := &revocationServer{
		forms:  make(chan url.Values, 1),
		revoke: func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":              "https://auth.example.test",
			"revocation_endpoint": s.URL + "/oauth/revoke",
		})
	})
	mux.HandleFunc("POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		select {
		case s.forms <- r.PostForm:
		default:
		}
		s.revoke(w)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *revocationServer) endpoints() oauth.Endpoints {
	return oauth.Endpoints{DiscoveryURL: s.URL + "/.well-known/openid-configuration"}
}

func TestRevokePostsTheRefreshTokenToTheDiscoveredEndpoint(t *testing.T) {
	s := newRevocationServer(t)

	err := oauth.Revoke(context.Background(), s.Client(), s.endpoints(), "oaiapp_issued", "refresh-1")

	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	var form url.Values
	select {
	case form = <-s.forms:
	default:
		t.Fatal("the revocation endpoint received no request")
	}
	want := url.Values{
		"token":           {"refresh-1"},
		"token_type_hint": {"refresh_token"},
		"client_id":       {"oaiapp_issued"},
	}
	if form.Encode() != want.Encode() {
		t.Fatalf("revocation form = %s, want %s", form.Encode(), want.Encode())
	}
}

func TestRevokeReportsANon200Answer(t *testing.T) {
	s := newRevocationServer(t)
	s.revoke = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}

	err := oauth.Revoke(context.Background(), s.Client(), s.endpoints(), "oaiapp_issued", "refresh-1")

	if err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("Revoke error = %v, want one naming invalid_client", err)
	}
}
