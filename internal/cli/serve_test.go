package cli_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServeForwardsModelsWithTheStoredAccessToken(t *testing.T) {
	h := newHarness(t)
	creds := signedIn()
	creds.ExpiresAt = time.Now().Add(time.Hour)
	h.saveCredentials(t, creds)
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r.URL.Path + " " + r.Header.Get("Authorization"):
		default:
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(upstream.Close)
	h.upstream = upstream.URL + "/v1"
	h.vars["OAI_PLAN_PROXY_API_KEY"] = "proxy-key"
	addr := freeAddr(t)
	h.vars["OAI_PLAN_PROXY_LISTEN"] = addr
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, []string{"serve"}, h.env())
	defer stopServe(t, cancel, done)
	waitForGet(t, "http://"+addr+"/healthz").Body.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer proxy-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK || string(body) != `{"object":"list","data":[]}` {
		t.Fatalf("response = %d %q, want the upstream list", resp.StatusCode, body)
	}
	select {
	case got := <-seen:
		if got != "/v1/models Bearer secret-access-token" {
			t.Fatalf("upstream saw %q, want /v1/models with the stored access token", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream received no request")
	}
}

// serveWithUpstream runs serve with args against a fake upstream that
// answers every request with 200, and returns the listen address.
func serveWithUpstream(t *testing.T, args ...string) string {
	t.Helper()
	h := newHarness(t)
	creds := signedIn()
	creds.ExpiresAt = time.Now().Add(time.Hour)
	h.saveCredentials(t, creds)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	h.upstream = upstream.URL + "/v1"
	addr := freeAddr(t)
	h.vars["OAI_PLAN_PROXY_LISTEN"] = addr
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, append([]string{"serve"}, args...), h.env())
	t.Cleanup(func() { stopServe(t, cancel, done) })
	waitForGet(t, "http://"+addr+"/healthz").Body.Close()
	return addr
}

func TestServeStrictRejectsPreviewLimitedParameters(t *testing.T) {
	addr := serveWithUpstream(t, "--strict")

	resp, err := http.Post("http://"+addr+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5","input":"hi","temperature":0.2}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 under --strict", resp.StatusCode)
	}
}

// stopServe cancels serve and waits for it to exit. It first drops the test
// client's idle connections: the client may have dialed a spare connection
// it never used, and Shutdown waits up to five seconds for a connection that
// has not sent a request.
func stopServe(t *testing.T, cancel context.CancelFunc, done <-chan int) {
	t.Helper()
	http.DefaultClient.CloseIdleConnections()
	cancel()
	waitExit(t, done)
}
