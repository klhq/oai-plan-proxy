package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/server"
)

// logBuffer collects JSON log lines from concurrent writers.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns the parsed records whose msg is msg.
func (b *logBuffer) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			found = append(found, rec)
		}
	}
	return found
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// loggedProxy builds the handler like proxy, with a JSON logger.
func loggedProxy(u *fakeUpstream, tok *fakeTokens, login auth.State) (http.Handler, *logBuffer) {
	logs := &logBuffer{}
	h := server.New(server.Options{
		Login:    fakeLogin{state: login},
		Tokens:   tok,
		Upstream: u.URL + "/v1",
		Client:   u.Client(),
		Logger:   slog.New(slog.NewJSONHandler(logs, nil)),
	})
	return h, logs
}

// oneRequestRecord returns the only request record, failing otherwise.
func oneRequestRecord(t *testing.T, logs *logBuffer) map[string]any {
	t.Helper()
	recs := logs.records(t, "request")
	if len(recs) != 1 {
		t.Fatalf("request records = %d, want 1; logs:\n%s", len(recs), logs)
	}
	return recs[0]
}

func TestEachV1RequestLogsOneRequestRecord(t *testing.T) {
	u := newUpstream(t, planModels(`{"models":[]}`))
	h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

	get(t, h, "/v1/models")

	rec := oneRequestRecord(t, logs)
	if _, ok := rec["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms = %v, want a number", rec["duration_ms"])
	}
	if rec["level"] != "INFO" || rec["method"] != "GET" || rec["path"] != "/v1/models" || rec["status"] != float64(200) {
		t.Fatalf("request record = %v", rec)
	}
}

func TestAHealthyHealthzProbeIsNotLogged(t *testing.T) {
	u := newUpstream(t, planModels(`{"models":[]}`))
	h, logs := loggedProxy(u, &fakeTokens{}, auth.StateValid)

	get(t, h, "/healthz")

	if recs := logs.records(t, "request"); len(recs) != 0 {
		t.Fatalf("request records = %v, want none", recs)
	}
}

func TestAFailedHealthzProbeIsLogged(t *testing.T) {
	u := newUpstream(t, planModels(`{"models":[]}`))
	h, logs := loggedProxy(u, &fakeTokens{}, auth.StateDead)

	get(t, h, "/healthz")

	rec := oneRequestRecord(t, logs)
	if rec["path"] != "/healthz" || rec["status"] != float64(503) {
		t.Fatalf("request record = %v", rec)
	}
}

func TestAServerErrorIsLoggedAsAWarning(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"down"}}`, http.StatusBadGateway)
	})
	h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

	get(t, h, "/v1/models")

	rec := oneRequestRecord(t, logs)
	if rec["level"] != "WARN" || rec["status"] != float64(502) {
		t.Fatalf("request record = %v", rec)
	}
}

func TestARequestTheCallerAbandonsIsLoggedAsCanceled(t *testing.T) {
	arrived := make(chan struct{})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	})
	h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/models", nil)
	errc := make(chan error, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached upstream")
	}

	cancel()

	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("client never returned")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(logs.records(t, "request")) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no request record; logs:\n%s", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec := oneRequestRecord(t, logs); rec["canceled"] != true {
		t.Fatalf("request record = %v, want canceled=true", rec)
	}
}

func TestAResponsesRequestLogsItsModelAndStreamMode(t *testing.T) {
	for _, tc := range []struct {
		body   string
		stream bool
	}{
		{`{"model":"gpt-5","input":"hi"}`, false},
		{`{"model":"gpt-5","input":"hi","stream":true}`, true},
	} {
		u := sseUpstream(t, evCreated, evDelta, evCompleted)
		h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

		post(t, h, "/v1/responses", tc.body)

		rec := oneRequestRecord(t, logs)
		if rec["model"] != "gpt-5" || rec["stream"] != tc.stream {
			t.Errorf("%s: request record = %v", tc.body, rec)
		}
	}
}

func TestAChatRequestLogsItsModelAndStreamMode(t *testing.T) {
	u := sseUpstream(t, evCreated, evDelta, evCompleted)
	h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

	post(t, h, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	rec := oneRequestRecord(t, logs)
	if rec["model"] != "gpt-5" || rec["stream"] != true {
		t.Fatalf("request record = %v", rec)
	}
}

func TestARequestLogsTheParametersItDropped(t *testing.T) {
	u := sseUpstream(t, evCreated, evDelta, evCompleted)
	h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

	post(t, h, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"seed":1,"stop":"x"}`)

	rec := oneRequestRecord(t, logs)
	dropped, _ := rec["dropped"].([]any)
	if len(dropped) != 2 || dropped[0] != "seed" || dropped[1] != "stop" {
		t.Fatalf("request record = %v, want dropped [seed stop]", rec)
	}
}

const evCompletedWithUsage = `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":3}}}}`

func TestACompletedResponseLogsItsTokenUsage(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"gpt-5","input":"hi"}`},
		{"/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`},
		{"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":true}`},
	} {
		u := sseUpstream(t, evCreated, evDelta, evCompletedWithUsage)
		h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

		post(t, h, tc.path, tc.body)

		rec := oneRequestRecord(t, logs)
		if rec["input_tokens"] != float64(10) || rec["output_tokens"] != float64(5) || rec["cached_tokens"] != float64(3) {
			t.Errorf("%s %s: request record = %v", tc.path, tc.body, rec)
		}
	}
}

func TestAnUpstreamErrorLogsItsStatusAndCode(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_value","message":"bad"}}`))
	})
	h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

	post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	rec := oneRequestRecord(t, logs)
	if rec["status"] != float64(400) || rec["upstream_status"] != float64(400) || rec["error_code"] != "invalid_value" {
		t.Fatalf("request record = %v", rec)
	}
}

func TestTheUsageLimitIsLoggedAsAWarningWithItsCode(t *testing.T) {
	httpLimit := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"` + usageLimit + `","message":"Plan usage limit reached."}}`))
	})
	eventLimit := sseUpstream(t, evCreated, `{"type":"error","code":"`+usageLimit+`","message":"Plan usage limit reached."}`)
	for name, u := range map[string]*fakeUpstream{"http": httpLimit, "event": eventLimit} {
		h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

		post(t, h, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)

		rec := oneRequestRecord(t, logs)
		if rec["level"] != "WARN" || rec["status"] != float64(429) || rec["error_code"] != usageLimit {
			t.Errorf("%s: request record = %v", name, rec)
		}
	}
}

func TestAFailureAfterOutputStartedIsLoggedAsAStreamError(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`},
		{"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":true}`},
	} {
		u := sseUpstream(t, evCreated, evDelta, evFailed)
		h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

		post(t, h, tc.path, tc.body)

		rec := oneRequestRecord(t, logs)
		if rec["level"] != "WARN" || rec["status"] != float64(200) ||
			rec["error_code"] != "server_error" || rec["stream_error"] != true {
			t.Errorf("%s: request record = %v", tc.path, rec)
		}
	}
}
