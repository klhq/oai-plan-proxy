package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/server"
)

// heldUpstream answers each request with a completed response, but only
// after release is closed. It signals entered when a request arrives.
func heldUpstream(t *testing.T) (u *fakeUpstream, entered <-chan struct{}, release chan struct{}) {
	t.Helper()
	in := make(chan struct{}, 8)
	release = make(chan struct{})
	u = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		in <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{evCreated, evDelta, evCompletedWithUsage} {
			_, _ = io.WriteString(w, sse(ev))
		}
	})
	return u, in, release
}

// limitedProxy builds the handler with a JSON logger and a limit of n
// model requests in flight.
func limitedProxy(u *fakeUpstream, n int) (http.Handler, *logBuffer) {
	logs := &logBuffer{}
	return server.New(server.Options{
		Login:         fakeLogin{state: auth.StateValid},
		Tokens:        &fakeTokens{current: "access-1"},
		Upstream:      u.URL + "/v1",
		Client:        u.Client(),
		MaxConcurrent: n,
		Logger:        slog.New(slog.NewJSONHandler(logs, nil)),
	}), logs
}

// holdOne starts a request that waits at the upstream and returns a channel
// that gets its status once it ends.
func holdOne(t *testing.T, h http.Handler, entered <-chan struct{}) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`)))
		done <- rec.Code
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the held request never reached upstream")
	}
	return done
}

func TestRequestsOverTheConcurrencyLimitGetA429AtOnce(t *testing.T) {
	u, entered, release := heldUpstream(t)
	h, _ := limitedProxy(u, 1)
	body := `{"model":"gpt-5","input":"hi"}`
	first := holdOne(t, h, entered)

	over := post(t, h, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)

	_, typ, code := openAIError(t, over)
	if over.Code != http.StatusTooManyRequests || typ != "rate_limit_error" || code != "concurrency_limit" ||
		over.Header().Get("Retry-After") != "1" || over.Header().Get("x-should-retry") != "" {
		t.Fatalf("over the limit: status = %d, type = %q, code = %q, Retry-After = %q, x-should-retry = %q",
			over.Code, typ, code, over.Header().Get("Retry-After"), over.Header().Get("x-should-retry"))
	}
	close(release)
	select {
	case c := <-first:
		if c != http.StatusOK {
			t.Fatalf("first request status = %d, want 200", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first request never finished")
	}
	if after := post(t, h, "/v1/responses", body); after.Code != http.StatusOK {
		t.Fatalf("after the first finished: status = %d, want 200", after.Code)
	}
}

func TestARequestOverTheConcurrencyLimitIsLoggedAsAWarning(t *testing.T) {
	u, entered, release := heldUpstream(t)
	h, logs := limitedProxy(u, 1)
	first := holdOne(t, h, entered)

	post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	close(release)
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("the held request never finished")
	}
	var rejected []map[string]any
	for _, rec := range logs.records(t, "request") {
		if rec["status"] == float64(http.StatusTooManyRequests) {
			rejected = append(rejected, rec)
		}
	}
	if len(rejected) != 1 || rejected[0]["level"] != "WARN" || rejected[0]["error_code"] != "concurrency_limit" {
		t.Fatalf("429 records = %v, want one WARN with error_code concurrency_limit", rejected)
	}
}
