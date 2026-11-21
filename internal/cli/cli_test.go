package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/cli"
)

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// waitForGet polls url until the server answers or the deadline passes.
func waitForGet(t *testing.T, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered %s: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type harness struct {
	stdout, stderr bytes.Buffer
	vars           map[string]string
}

func newHarness(t *testing.T) *harness {
	return &harness{vars: map[string]string{"OAI_PLAN_PROXY_DATA_DIR": t.TempDir()}}
}

func (h *harness) env() cli.Env {
	return cli.Env{
		Stdout: &h.stdout,
		Stderr: &h.stderr,
		Getenv: func(k string) string { return h.vars[k] },
		UserConfigDir: func() (string, error) {
			panic("tests set OAI_PLAN_PROXY_DATA_DIR")
		},
	}
}

func TestUnknownCommandPrintsUsageAndExits2(t *testing.T) {
	h := newHarness(t)

	code := cli.Run(context.Background(), []string{"frobnicate"}, h.env())

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(h.stderr.String(), "usage") {
		t.Fatalf("stderr = %q, want usage text", h.stderr.String())
	}
}

func TestServeAnswersHealthzWithoutLoginAndStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	addr := freeAddr(t)
	h.vars["OAI_PLAN_PROXY_LISTEN"] = addr
	ctx, cancel := context.WithCancel(context.Background())
	var (
		wg   sync.WaitGroup
		code int
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		code = cli.Run(ctx, []string{"serve"}, h.env())
	}()

	resp := waitForGet(t, "http://"+addr+"/healthz")
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	cancel()
	wg.Wait()

	if resp.StatusCode != http.StatusServiceUnavailable || body["login"] != "missing" {
		t.Fatalf("healthz = %d %v, want 503 with login missing", resp.StatusCode, body)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, h.stderr.String())
	}
}
