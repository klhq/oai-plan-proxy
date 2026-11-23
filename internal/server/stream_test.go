package server_test

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
)

// sse formats one server-sent event carrying data.
func sse(data string) string {
	return "event: " + eventType(data) + "\ndata: " + data + "\n\n"
}

// eventType pulls the "type" value out of an event payload.
func eventType(data string) string {
	_, rest, _ := strings.Cut(data, `"type":"`)
	typ, _, _ := strings.Cut(rest, `"`)
	return typ
}

const (
	evCreated   = `{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`
	evDelta     = `{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hel"}`
	evDelta2    = `{"type":"response.output_text.delta","item_id":"msg_1","delta":"lo"}`
	evCompleted = `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello"}]}]}}`
)

// readEvent reads one blank-line-terminated SSE event, failing after a
// timeout so a proxy that buffers cannot hang the test.
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		for {
			line, err := r.ReadString('\n')
			b.WriteString(line)
			if err != nil || line == "\n" {
				got <- b.String()
				return
			}
		}
	}()
	select {
	case ev := <-got:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event reached the caller within 3s; the proxy is buffering")
		return ""
	}
}

func TestAStreamingCallerGetsEachEventUnchangedAsItArrives(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(evCreated)+sse(evDelta))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = io.WriteString(w, sse(evDelta2)+sse(evCompleted))
	})
	srv := httptest.NewServer(proxy(u, &fakeTokens{current: "access-1"}, ""))
	t.Cleanup(srv.Close)

	resp := postWithin(t, srv.URL+"/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response = %d %q, want 200 text/event-stream", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{evCreated, evDelta} {
		if got := readEvent(t, r); got != sse(want) {
			t.Fatalf("event = %q, want %q", got, sse(want))
		}
	}
	close(release)
	for _, want := range []string{evDelta2, evCompleted} {
		if got := readEvent(t, r); got != sse(want) {
			t.Fatalf("event = %q, want %q", got, sse(want))
		}
	}
}

// postWithin posts body and fails if the response headers take longer than
// 3s, so a proxy that holds the whole stream cannot hang the test.
func postWithin(t *testing.T, url, body string) *http.Response {
	t.Helper()
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		got <- result{resp, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.resp
	case <-time.After(3 * time.Second):
		t.Fatal("no response headers within 3s; the proxy is holding the stream")
		return nil
	}
}

// sseUpstream answers every request with the given events.
func sseUpstream(t *testing.T, events ...string) *fakeUpstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			_, _ = io.WriteString(w, sse(ev))
		}
	})
}

func TestANonStreamingCallerGetsTheCompletedResponseObject(t *testing.T) {
	u := sseUpstream(t, evCreated, evDelta, evDelta2, evCompleted)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	want := `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello"}]}]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("response = %d %q, want 200 with the completed response object", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

func TestANonStreamingCallerGetsA502WhenTheStreamEndsWithoutCompleting(t *testing.T) {
	u := sseUpstream(t, evCreated, evDelta)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %q, want 502", rec.Code, rec.Body.String())
	}
	openAIError(t, rec)
}

const evFailed = `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"The model failed."}}}`

func TestAFailureBeforeAnyOutputIsARealHTTPError(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5","input":"hi","stream":true}`,
		`{"model":"gpt-5","input":"hi"}`,
	} {
		u := sseUpstream(t, evCreated, evFailed)
		h := proxy(u, &fakeTokens{current: "access-1"}, "")

		rec := post(t, h, "/v1/responses", body)

		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d %q, want 502 instead of a 200 stream", body, rec.Code, rec.Body.String())
			continue
		}
		if msg, _, code := openAIError(t, rec); code != "server_error" || msg != "The model failed." {
			t.Errorf("%s: error = %q %q, want upstream's server_error and message", body, code, msg)
		}
	}
}

func TestAStreamThatEndsBeforeAnyOutputIsA502(t *testing.T) {
	u := sseUpstream(t, evCreated)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %q, want 502", rec.Code, rec.Body.String())
	}
	openAIError(t, rec)
}

const usageLimit = "subscription_sharing_usage_limit_exceeded"

// assertUsageLimit checks the answer Bifrost needs to fall back: a 429 that
// says not to retry.
func assertUsageLimit(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d %q, want 429", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-should-retry"); got != "false" {
		t.Fatalf("x-should-retry = %q, want false", got)
	}
	if _, _, code := openAIError(t, rec); code != usageLimit {
		t.Fatalf("code = %q, want %s", code, usageLimit)
	}
}

func TestTheUsageLimitAsAnHTTPErrorIsANoRetry429(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit","code":"`+usageLimit+`","message":"Plan usage limit reached."}}`)
		})
		h := proxy(u, &fakeTokens{current: "access-1"}, "")

		rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)

		assertUsageLimit(t, rec)
	}
}

func TestTheUsageLimitAsAFailedEventBeforeOutputIsANoRetry429(t *testing.T) {
	failed := `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"` + usageLimit + `","message":"Plan usage limit reached."}}}`
	for _, body := range []string{
		`{"model":"gpt-5","input":"hi","stream":true}`,
		`{"model":"gpt-5","input":"hi"}`,
	} {
		u := sseUpstream(t, evCreated, failed)
		h := proxy(u, &fakeTokens{current: "access-1"}, "")

		rec := post(t, h, "/v1/responses", body)

		assertUsageLimit(t, rec)
	}
}

// The plan route sends this sequence once the plan's usage is spent, as seen
// on a live deployment on 2026-11-23.
func TestTheUsageLimitAfterResponseMetadataIsStillANoRetry429(t *testing.T) {
	inProgress := `{"type":"response.in_progress","response":{"id":"resp_1","status":"in_progress"}}`
	metadata := `{"type":"response.metadata","response_id":"resp_1","metadata":{"openai_chatgpt_moderation_metadata":{"prompt":{},"generation":{},"tool_call":{},"tool_response":{}}}}`
	errEvent := `{"type":"error","error":{"type":"invalid_request_error","code":"` + usageLimit + `","message":"Plan usage limit reached."}}`
	u := sseUpstream(t, evCreated, inProgress, metadata, errEvent)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)

	assertUsageLimit(t, rec)
}

func TestAnErrorEventBeforeOutputIsARealHTTPError(t *testing.T) {
	cases := []struct {
		event string
		check func(*testing.T, *httptest.ResponseRecorder)
	}{
		{`{"type":"error","code":"` + usageLimit + `","message":"Plan usage limit reached."}`, assertUsageLimit},
		{`{"type":"error","code":"server_error","message":"Upstream broke."}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d %q, want 502", rec.Code, rec.Body.String())
			}
			if msg, _, _ := openAIError(t, rec); msg != "Upstream broke." {
				t.Fatalf("message = %q, want upstream's", msg)
			}
		}},
	}
	for _, c := range cases {
		u := sseUpstream(t, evCreated, c.event)
		h := proxy(u, &fakeTokens{current: "access-1"}, "")

		rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)

		c.check(t, rec)
	}
}

func TestOtherUpstreamHTTPErrorsPassThroughUnchanged(t *testing.T) {
	const errBody = `{"error":{"type":"invalid_request_error","code":"model_not_found","message":"No such model."}}`
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errBody)
	})
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/responses", `{"model":"nope","input":"hi","stream":true}`)

	if rec.Code != http.StatusBadRequest || rec.Body.String() != errBody {
		t.Fatalf("response = %d %q, want upstream's 400 and body", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("x-should-retry") != "" {
		t.Fatal("x-should-retry was set on an ordinary error")
	}
}

func TestAFailureAfterOutputStartedPassesThroughTheStream(t *testing.T) {
	u := sseUpstream(t, evCreated, evDelta, evFailed)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)

	if want := sse(evCreated) + sse(evDelta) + sse(evFailed); rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("response = %d %q, want 200 with the stream as upstream sent it", rec.Code, rec.Body.String())
	}
}

// codexUsageLimit is the usage-limit error Codex recognizes: the type names
// the limit and the reset time is in Unix seconds.
const codexUsageLimit = `{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_at":1795000000}`

func TestTheCodexUsageLimitAsAnHTTPErrorIsANoRetry429(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":`+codexUsageLimit+`}`)
	})
	h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)

	assertUsageLimit(t, rec)
	if got := oneRequestRecord(t, logs); got["level"] != "WARN" || got["error_code"] != "usage_limit_reached" {
		t.Fatalf("request record = %v, want WARN with error_code usage_limit_reached", got)
	}
}

func TestTheCodexUsageLimitAsAnEventBeforeOutputIsANoRetry429(t *testing.T) {
	for name, ev := range map[string]string{
		"failed response": `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":` + codexUsageLimit + `}}`,
		"error event":     `{"type":"error","status":429,"error":` + codexUsageLimit + `}`,
	} {
		h, logs := loggedProxy(sseUpstream(t, evCreated, ev), &fakeTokens{current: "access-1"}, auth.StateValid)

		rec := post(t, h, "/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)

		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("x-should-retry") != "false" {
			t.Errorf("%s: status = %d, x-should-retry = %q, want 429 and false", name, rec.Code, rec.Header().Get("x-should-retry"))
			continue
		}
		if got := oneRequestRecord(t, logs); got["error_code"] != "usage_limit_reached" {
			t.Errorf("%s: request record = %v, want error_code usage_limit_reached", name, got)
		}
	}
}

// limitUpstream answers 429 with the usage-limit error body after setHeaders
// adds its headers.
func limitUpstream(t *testing.T, body string, setHeaders func(http.Header)) *fakeUpstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w.Header())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, body)
	})
}

// assertRetryAfter checks that the answer and its record both say to come
// back in about want seconds.
func assertRetryAfter(t *testing.T, rec *httptest.ResponseRecorder, logs *logBuffer, want int) {
	t.Helper()
	got, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || got < want-1 || got > want+1 {
		t.Fatalf("Retry-After = %q, want about %d", rec.Header().Get("Retry-After"), want)
	}
	if r := oneRequestRecord(t, logs); r["retry_after_s"] != float64(got) {
		t.Fatalf("retry_after_s = %v, want %d", r["retry_after_s"], got)
	}
}

func TestTheUsageLimitSaysWhenTheLimitResets(t *testing.T) {
	resetsAt := time.Now().Add(time.Hour).Unix()
	body := `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":` + strconv.FormatInt(resetsAt, 10) + `}}`
	h, logs := loggedProxy(limitUpstream(t, body, func(http.Header) {}), &fakeTokens{current: "access-1"}, auth.StateValid)

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	assertUsageLimit(t, rec)
	assertRetryAfter(t, rec, logs, 3600)
}

func TestWithoutAResetTimeTheUsageLimitWaitsForTheSpentWindow(t *testing.T) {
	in := func(d time.Duration) string { return strconv.FormatInt(time.Now().Add(d).Unix(), 10) }
	for _, tc := range []struct {
		name               string
		primary, secondary string
		want               int
	}{
		{"short window spent", "100", "30", 600},
		{"both windows spent", "100", "100.0", 7200},
	} {
		u := limitUpstream(t, `{"error":{"code":"`+usageLimit+`","message":"Plan usage limit reached."}}`, func(h http.Header) {
			h.Set("x-codex-primary-used-percent", tc.primary)
			h.Set("x-codex-primary-reset-at", in(10*time.Minute))
			h.Set("x-codex-secondary-used-percent", tc.secondary)
			h.Set("x-codex-secondary-reset-at", in(2*time.Hour))
		})
		h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

		rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

		t.Run(tc.name, func(t *testing.T) { assertRetryAfter(t, rec, logs, tc.want) })
	}
}

func TestTheUsageLimitHasNoRetryAfterWhenTheResetIsUnknown(t *testing.T) {
	past := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	for name, u := range map[string]*fakeUpstream{
		"reset time passed": limitUpstream(t, `{"error":{"type":"usage_limit_reached","resets_at":`+past+`}}`, func(http.Header) {}),
		"no window spent": limitUpstream(t, `{"error":{"type":"usage_limit_reached"}}`, func(h http.Header) {
			h.Set("x-codex-primary-used-percent", "40")
			h.Set("x-codex-primary-reset-at", future)
		}),
	} {
		h, logs := loggedProxy(u, &fakeTokens{current: "access-1"}, auth.StateValid)

		rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

		_, logged := oneRequestRecord(t, logs)["retry_after_s"]
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "" || logged {
			t.Errorf("%s: status = %d, Retry-After = %q, retry_after_s logged = %v; want 429 and neither",
				name, rec.Code, rec.Header().Get("Retry-After"), logged)
		}
	}
}

func TestAnOddResetTimeStillGetsTheUsageLimitAnswer(t *testing.T) {
	soon := float64(time.Now().Add(time.Hour).Unix()) + 0.5
	for name, resetsAt := range map[string]string{
		"fraction": strconv.FormatFloat(soon, 'f', 1, 64),
		"string":   `"tomorrow"`,
	} {
		body := `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":` + resetsAt + `}}`
		h := proxy(limitUpstream(t, body, func(http.Header) {}), &fakeTokens{current: "access-1"}, "")

		rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

		t.Run(name, func(t *testing.T) { assertUsageLimit(t, rec) })
	}
}
