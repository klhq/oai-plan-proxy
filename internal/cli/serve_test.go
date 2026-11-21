package cli_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	defer func() { cancel(); waitExit(t, done) }()
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
