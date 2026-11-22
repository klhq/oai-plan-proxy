package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

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
