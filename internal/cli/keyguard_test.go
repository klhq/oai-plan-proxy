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

func TestAllowNoKeyServesAnOpenListenerWithAWarning(t *testing.T) {
	h := newHarness(t)
	port := portOf(t, freeAddr(t))
	h.vars["OAI_PLAN_PROXY_LISTEN"] = "0.0.0.0:" + port
	h.vars["OAI_PLAN_PROXY_ALLOW_NO_KEY"] = "1"
	h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "json"
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, []string{"serve"}, h.env())
	waitForGet(t, "http://127.0.0.1:"+port+"/healthz").Body.Close()

	stopServe(t, cancel, done)

	warns := logRecords(t, h.stderr.String(), "no API key")
	if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["addr"] != "0.0.0.0:"+port {
		t.Fatalf("no API key records = %v, want one WARN with addr 0.0.0.0:%s", warns, port)
	}
	if l := logRecords(t, h.stderr.String(), "listening"); len(l) != 1 || l[0]["allow_no_key"] != true {
		t.Fatalf("listening records = %v, want allow_no_key true", l)
	}
}

func TestServeLogsItsConcurrencyLimit(t *testing.T) {
	for value, want := range map[string]float64{"": 0, "3": 3} {
		h := newHarness(t)
		h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "json"
		h.vars["OAI_PLAN_PROXY_LISTEN"] = freeAddr(t)
		h.vars["OAI_PLAN_PROXY_MAX_CONCURRENT"] = value
		ctx, cancel := context.WithCancel(context.Background())
		done := runAsync(ctx, []string{"serve"}, h.env())
		waitForGet(t, "http://"+h.vars["OAI_PLAN_PROXY_LISTEN"]+"/healthz").Body.Close()

		stopServe(t, cancel, done)

		if l := logRecords(t, h.stderr.String(), "listening"); len(l) != 1 || l[0]["max_concurrent"] != want {
			t.Errorf("%q: listening records = %v, want max_concurrent %v", value, l, want)
		}
	}
}

func TestServeLogsItsUsageLimitPause(t *testing.T) {
	for value, want := range map[string]string{"": "30s", "0": "0s", "2m": "2m0s"} {
		h := newHarness(t)
		h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "json"
		h.vars["OAI_PLAN_PROXY_LISTEN"] = freeAddr(t)
		h.vars["OAI_PLAN_PROXY_USAGE_LIMIT_PAUSE"] = value
		ctx, cancel := context.WithCancel(context.Background())
		done := runAsync(ctx, []string{"serve"}, h.env())
		waitForGet(t, "http://"+h.vars["OAI_PLAN_PROXY_LISTEN"]+"/healthz").Body.Close()

		stopServe(t, cancel, done)

		if l := logRecords(t, h.stderr.String(), "listening"); len(l) != 1 || l[0]["usage_limit_pause"] != want {
			t.Errorf("%q: listening records = %v, want usage_limit_pause %q", value, l, want)
		}
	}
}
