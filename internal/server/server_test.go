package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/server"
)

type fakeLogin struct{ state auth.State }

func (f fakeLogin) State() auth.State { return f.state }

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v: %q", err, rec.Body.String())
	}
	return body
}

func TestHealthzReportsMissingLoginAs503(t *testing.T) {
	h := server.New(server.Options{Login: fakeLogin{state: auth.StateMissing}})

	rec := get(t, h, "/healthz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := decode(t, rec)["login"]; got != "missing" {
		t.Fatalf("login = %v, want missing", got)
	}
}

func TestHealthzReportsValidLoginAs200(t *testing.T) {
	h := server.New(server.Options{Login: fakeLogin{state: auth.StateValid}})

	rec := get(t, h, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	if body["status"] != "ok" || body["login"] != "valid" {
		t.Fatalf("body = %v, want status ok and login valid", body)
	}
}
