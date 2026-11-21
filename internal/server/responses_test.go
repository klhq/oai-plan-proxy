package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

func TestResponsesAreSentUpstreamSanitizedAndNameTheDroppedParameters(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if r.Header.Get("Content-Type") != "application/json" {
			body["content_type_was"] = r.Header.Get("Content-Type")
		}
		bodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	})
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","temperature":0.2,"user":"u1","store":true}`)

	if got := rec.Header().Get("x-oai-plan-proxy-dropped-params"); got != "temperature,user" {
		t.Errorf("x-oai-plan-proxy-dropped-params = %q, want temperature,user", got)
	}
	var body map[string]any
	select {
	case body = <-bodies:
	default:
		t.Fatalf("upstream received no request; response %d %q", rec.Code, rec.Body.String())
	}
	if body["store"] != false || body["stream"] != true || body["temperature"] != nil || body["content_type_was"] != nil {
		t.Fatalf("upstream body = %v, want the sanitized JSON body", body)
	}
	if calls := u.received(); calls[0] != (upstreamCall{"POST", "/v1/responses", "Bearer access-1"}) {
		t.Fatalf("upstream call = %+v, want POST /v1/responses with the OAuth token", calls[0])
	}
}

func TestAnUnservableResponsesRequestGetsA400NamingTheField(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	h := proxy(u, &fakeTokens{current: "access-1"}, "")

	rec := post(t, h, "/v1/responses", `{"model":"gpt-5","input":[],"tools":[{"type":"code_interpreter"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Param != "tools[0]" || body.Error.Type != "invalid_request_error" || !strings.Contains(body.Error.Message, "code_interpreter") {
		t.Fatalf("error = %+v, want invalid_request_error with param tools[0] naming code_interpreter", body.Error)
	}
	if calls := u.received(); len(calls) != 0 {
		t.Fatalf("upstream received %+v, want nothing for a rejected request", calls)
	}
}

func TestAnOversizedResponsesBodyIsRejectedWithoutForwarding(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	h := proxy(u, &fakeTokens{current: "access-1"}, "")
	body := `{"model":"gpt-5","input":"` + strings.Repeat("a", 33<<20) + `"}`

	rec := post(t, h, "/v1/responses", body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if calls := u.received(); len(calls) != 0 {
		t.Fatalf("upstream received %d requests, want none", len(calls))
	}
}
