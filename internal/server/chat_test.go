package server_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/server"
)

// capturingUpstream answers every request with events and sends each
// decoded request body to the returned channel.
func capturingUpstream(t *testing.T, events ...string) (*fakeUpstream, <-chan map[string]any) {
	t.Helper()
	bodies := make(chan map[string]any, 4)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		bodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			_, _ = io.WriteString(w, sse(ev))
		}
	})
	return u, bodies
}

const helloCompleted = `{"type":"response.completed","response":{"id":"resp_1","created_at":1763800000,"model":"gpt-a","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]}}`

func TestAChatCompletionIsServedThroughTheResponsesRoute(t *testing.T) {
	u, bodies := capturingUpstream(t, helloCompleted)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/chat/completions", `{"model":"gpt-a","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"max_tokens":5,"temperature":0.2}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var c struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || c.Object != "chat.completion" || len(c.Choices) != 1 || c.Choices[0].Message.Content != "Hello" {
		t.Fatalf("body = %s, want a chat.completion saying Hello", rec.Body.String())
	}
	if got := rec.Header().Get("x-oai-plan-proxy-dropped-params"); got != "max_tokens,temperature" {
		t.Errorf("x-oai-plan-proxy-dropped-params = %q, want max_tokens,temperature", got)
	}
	var body map[string]any
	select {
	case body = <-bodies:
	default:
		t.Fatal("upstream received no request")
	}
	if body["store"] != false || body["stream"] != true || body["messages"] != nil || body["input"] == nil || body["temperature"] != nil {
		t.Fatalf("upstream body = %v, want a sanitized Responses request", body)
	}
	if calls := u.received(); calls[0] != (upstreamCall{"POST", "/v1/responses", "Bearer access-1"}) {
		t.Fatalf("upstream call = %+v, want POST /v1/responses with the OAuth token", calls[0])
	}
}

func TestStrictModeRejectsAChatFieldItWouldDrop(t *testing.T) {
	u, bodies := capturingUpstream(t, helloCompleted)
	h := server.New(server.Options{
		Login: fakeLogin{state: auth.StateValid}, Tokens: &fakeTokens{current: "access-1"},
		Upstream: u.URL + "/v1", Client: u.Client(), Strict: true,
	})

	rec := post(t, h, "/v1/chat/completions", `{"model":"gpt-a","messages":[{"role":"user","content":"hi"}],"stop":["\n"]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if _, _, code := openAIError(t, rec); code != "unsupported_parameter" {
		t.Errorf("code = %q, want unsupported_parameter", code)
	}
	if param := decode(t, rec)["error"].(map[string]any)["param"]; param != "stop" {
		t.Errorf("param = %v, want stop", param)
	}
	select {
	case b := <-bodies:
		t.Fatalf("upstream received %v, want nothing sent", b)
	default:
	}
}

func TestAChatRequestTheProxyCannotHonorGetsA400NamingTheField(t *testing.T) {
	u, bodies := capturingUpstream(t, helloCompleted)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/chat/completions", `{"model":"gpt-a","messages":[{"role":"user","content":"hi"}],"n":3}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if param := decode(t, rec)["error"].(map[string]any)["param"]; param != "n" {
		t.Errorf("param = %v, want n", param)
	}
	select {
	case b := <-bodies:
		t.Fatalf("upstream received %v, want nothing sent", b)
	default:
	}
}

func TestThe404ForOtherRoutesNamesChatCompletionsAsServed(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := get(t, h, "/v1/embeddings")

	if msg, _, _ := openAIError(t, rec); !strings.Contains(msg, "/v1/chat/completions") {
		t.Fatalf("message = %q, want it to list /v1/chat/completions", msg)
	}
}

const (
	createdEvent = `{"type":"response.created","response":{"id":"resp_1","created_at":1763800000,"model":"gpt-a","status":"in_progress","output":[]}}`
	helloDelta   = `{"type":"response.output_text.delta","delta":"Hello"}`
)

// chatData returns the data payloads of a Chat Completions event stream.
func chatData(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		data, ok := strings.CutPrefix(block, "data: ")
		if !ok {
			t.Fatalf("stream block %q is not a data line", block)
		}
		out = append(out, data)
	}
	return out
}

func TestAStreamingChatCallerGetsChunksEndingWithDone(t *testing.T) {
	u := sseUpstream(t, createdEvent, helloDelta, helloCompleted)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/chat/completions", `{"model":"gpt-a","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d Content-Type %q, want 200 text/event-stream", rec.Code, rec.Header().Get("Content-Type"))
	}
	data := chatData(t, rec.Body.String())
	if len(data) != 4 || data[3] != "[DONE]" {
		t.Fatalf("stream = %q, want role, content, finish, [DONE]", data)
	}
	if !strings.Contains(data[1], `"content":"Hello"`) || !strings.Contains(data[2], `"finish_reason":"stop"`) {
		t.Fatalf("stream = %q, want Hello then finish_reason stop", data)
	}
}

func TestAChatUsageLimitBeforeOutputIsANoRetry429InBothModes(t *testing.T) {
	failed := `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"` + usageLimit + `","message":"Plan usage limit reached."}}}`
	for _, body := range []string{
		`{"model":"gpt-a","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		`{"model":"gpt-a","messages":[{"role":"user","content":"hi"}]}`,
	} {
		u := sseUpstream(t, createdEvent, failed)
		h := proxy(u, &fakeTokens{current: "access-1"}, "")

		rec := post(t, h, "/v1/chat/completions", body)

		assertUsageLimit(t, rec)
	}
}

func TestAChatStreamThatEndsBeforeOutputIsA502(t *testing.T) {
	u := sseUpstream(t, createdEvent)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/chat/completions", `{"model":"gpt-a","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d %q, want 502", rec.Code, rec.Body.String())
	}
	if _, _, code := openAIError(t, rec); code != "upstream_incomplete" {
		t.Fatalf("code = %q, want upstream_incomplete", code)
	}
}

func TestAFailureAfterChatOutputStartedEndsWithAnErrorLineAndNoDone(t *testing.T) {
	failed := `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"The model crashed."}}}`
	u := sseUpstream(t, createdEvent, helloDelta, failed)
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/chat/completions", `{"model":"gpt-a","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	data := chatData(t, rec.Body.String())
	last := data[len(data)-1]
	var e struct {
		Error struct {
			Message, Type, Code string
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(last), &e); err != nil || e.Error.Code != "server_error" || e.Error.Message != "The model crashed." {
		t.Fatalf("last data = %q, want an error object with the upstream code and message", last)
	}
	if e.Error.Type == "" {
		t.Errorf("error type is empty, want one")
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("stream contains [DONE]; a failed stream must not look complete")
	}
}

func TestStreamedChatChunksArriveBeforeTheUpstreamFinishes(t *testing.T) {
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
		_, _ = io.WriteString(w, sse(createdEvent)+sse(helloDelta))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = io.WriteString(w, sse(helloCompleted))
	})
	srv := httptest.NewServer(proxy(u, &fakeTokens{current: "access-1"}, ""))
	t.Cleanup(srv.Close)

	resp := postWithin(t, srv.URL+"/v1/chat/completions", `{"model":"gpt-a","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	for _, want := range []string{`"role":"assistant"`, `"content":"Hello"`} {
		if got := readEvent(t, r); !strings.Contains(got, want) {
			t.Fatalf("chunk = %q, want it to contain %s", got, want)
		}
	}
	close(release)
	if got := readEvent(t, r); !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("chunk = %q, want the finish chunk", got)
	}
}
