package cli_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/cli"
)

// healthz serves /healthz with status and returns its listen address.
func healthz(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestHealthcheckExitsZeroOnlyWhenHealthzAnswers200(t *testing.T) {
	for name, tc := range map[string]struct {
		addr string
		want int
	}{
		"healthy":     {healthz(t, http.StatusOK), 0},
		"login dead":  {healthz(t, http.StatusServiceUnavailable), 1},
		"not running": {freeAddr(t), 1},
	} {
		h := newHarness(t)
		h.vars["OAI_PLAN_PROXY_LISTEN"] = tc.addr

		if code := cli.Run(context.Background(), []string{"healthcheck"}, h.env()); code != tc.want {
			t.Errorf("%s: exit code = %d, want %d; stderr: %s", name, code, tc.want, h.stderr.String())
		}
	}
}
