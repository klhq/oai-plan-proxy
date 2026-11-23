package cli_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// portOf returns the port of a host:port address.
func portOf(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestServeRefusesAnOpenListenerWithoutAnAPIKey(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "", "[::]", "proxy.example.test"} {
		t.Run("host "+host, func(t *testing.T) {
			h := newHarness(t)
			port := portOf(t, freeAddr(t))
			h.vars["OAI_PLAN_PROXY_LISTEN"] = host + ":" + port

			code := waitExit(t, runAsync(context.Background(), []string{"serve"}, h.env()))

			for _, name := range []string{"OAI_PLAN_PROXY_API_KEY", "OAI_PLAN_PROXY_API_KEY_FILE", "OAI_PLAN_PROXY_ALLOW_NO_KEY"} {
				if !strings.Contains(h.stderr.String(), name) {
					t.Errorf("stderr = %q, want it to name %s", h.stderr.String(), name)
				}
			}
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
				c.Close()
				t.Fatalf("something listens on port %s after the refusal", port)
			}
		})
	}
}
