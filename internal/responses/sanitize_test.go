package responses_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// fields decodes a sanitized body into its top-level raw fields.
func fields(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("sanitized body is not a JSON object: %v: %s", err, body)
	}
	return m
}

func sanitize(t *testing.T, body string, strict bool) responses.Request {
	t.Helper()
	req, err := responses.Sanitize([]byte(body), strict)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	return req
}

func TestStoreAndStreamAreForcedOn(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5","input":[]}`,
		`{"model":"gpt-5","input":[],"store":true,"stream":false}`,
	} {
		req := sanitize(t, body, false)

		f := fields(t, req.Body)
		if string(f["store"]) != "false" || string(f["stream"]) != "true" {
			t.Errorf("%s: store=%s stream=%s, want store=false stream=true", body, f["store"], f["stream"])
		}
	}
}

func TestFieldsTheProxyDoesNotKnowKeepTheirExactValues(t *testing.T) {
	body := `{"model":"gpt-5","input":[],"future_knob":1.0e2,"reasoning":{"effort":"high","summary":"auto"},"note":"café","big":12345678901234567890}`

	req := sanitize(t, body, false)

	f := fields(t, req.Body)
	want := map[string]string{
		"future_knob": `1.0e2`,
		"reasoning":   `{"effort":"high","summary":"auto"}`,
		"note":        `"café"`,
		"big":         `12345678901234567890`,
	}
	for k, v := range want {
		if string(f[k]) != v {
			t.Errorf("%s = %s, want %s unchanged", k, f[k], v)
		}
	}
}

// previewLimited is the preview-limits list of parameters the plan route
// rejects, in sorted order.
var previewLimited = []string{
	"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata",
	"moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier",
	"temperature", "top_logprobs", "top_p", "truncation", "user",
}

func TestPreviewLimitedParametersAreDroppedAndNamed(t *testing.T) {
	in := map[string]any{"model": "gpt-5", "input": []any{}, "instructions": "be brief"}
	for _, name := range previewLimited {
		in[name] = 1
	}
	body, _ := json.Marshal(in)

	req := sanitize(t, string(body), false)

	f := fields(t, req.Body)
	for _, name := range previewLimited {
		if _, ok := f[name]; ok {
			t.Errorf("%s reached the forwarded body", name)
		}
	}
	if string(f["instructions"]) != `"be brief"` {
		t.Errorf("instructions = %s, want it kept", f["instructions"])
	}
	if !slices.Equal(req.Dropped, previewLimited) {
		t.Fatalf("Dropped = %v, want the sorted list %v", req.Dropped, previewLimited)
	}
}

// rejection runs Sanitize and returns the request error it must produce.
func rejection(t *testing.T, body string, strict bool) *responses.Error {
	t.Helper()
	_, err := responses.Sanitize([]byte(body), strict)
	var reqErr *responses.Error
	if !errors.As(err, &reqErr) {
		t.Fatalf("Sanitize error = %v, want a *responses.Error", err)
	}
	return reqErr
}

func TestStrictModeRejectsPreviewLimitedParameters(t *testing.T) {
	err := rejection(t, `{"model":"gpt-5","input":[],"top_p":0.9,"temperature":0.2}`, true)

	if err.Param != "temperature" {
		t.Errorf("Param = %q, want the first sorted name temperature", err.Param)
	}
	for _, name := range []string{"temperature", "top_p"} {
		if !strings.Contains(err.Message, name) {
			t.Errorf("Message = %q, want it to name %s", err.Message, name)
		}
	}
}

func TestPreviousResponseIDIsRejected(t *testing.T) {
	err := rejection(t, `{"model":"gpt-5","input":[],"previous_response_id":"resp_123"}`, false)

	if err.Param != "previous_response_id" || !strings.Contains(err.Message, "full") {
		t.Fatalf("error = %+v, want previous_response_id named and a hint to send the full history", err)
	}
}

func TestANullPreviousResponseIDIsTreatedAsAbsent(t *testing.T) {
	req := sanitize(t, `{"model":"gpt-5","input":[],"previous_response_id":null}`, false)

	if _, ok := fields(t, req.Body)["previous_response_id"]; ok {
		t.Fatal("previous_response_id: null reached the forwarded body")
	}
}

func TestAStringInputIsWrappedAsOneUserMessage(t *testing.T) {
	req := sanitize(t, `{"model":"gpt-5","input":"Say hi"}`, false)

	got := string(fields(t, req.Body)["input"])
	if got != `[{"content":"Say hi","role":"user"}]` {
		t.Fatalf("input = %s, want one user message carrying the string", got)
	}
}

func TestSystemRoleInputItemsBecomeDeveloperItems(t *testing.T) {
	body := `{"model":"gpt-5","input":[` +
		`{"type":"message","role":"system","content":[{"type":"input_text","text":"Be terse."}]},` +
		`{"role":"user","content":"Hi"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"{\"ok\":1.0e0}"}]}`

	req := sanitize(t, body, false)

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(fields(t, req.Body)["input"], &items); err != nil {
		t.Fatal(err)
	}
	if string(items[0]["role"]) != `"developer"` {
		t.Errorf("first item role = %s, want developer", items[0]["role"])
	}
	if string(items[0]["content"]) != `[{"type":"input_text","text":"Be terse."}]` {
		t.Errorf("first item content = %s, want it unchanged", items[0]["content"])
	}
	if string(items[1]["role"]) != `"user"` {
		t.Errorf("second item role = %s, want user kept", items[1]["role"])
	}
	if string(items[2]["output"]) != `"{\"ok\":1.0e0}"` {
		t.Errorf("function output = %s, want it unchanged", items[2]["output"])
	}
}

func TestHostedToolsTheRouteCannotRunAreRejectedByType(t *testing.T) {
	for _, typ := range []string{
		"image_generation", "file_search", "code_interpreter",
		"computer", "computer_use_preview", "computer_use", "mcp", "tool_search",
	} {
		body := `{"model":"gpt-5","input":[],"tools":[{"type":"function","name":"f"},{"type":"` + typ + `"}]}`

		err := rejection(t, body, false)

		if err.Param != "tools[1]" || !strings.Contains(err.Message, typ) {
			t.Errorf("%s: error = %+v, want tools[1] named with the type", typ, err)
		}
	}
}

func TestFunctionCustomAndWebSearchToolsPassUnchanged(t *testing.T) {
	tools := `[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"custom","name":"patch"},{"type":"web_search"}]`

	req := sanitize(t, `{"model":"gpt-5","input":[],"tools":`+tools+`}`, false)

	if got := string(fields(t, req.Body)["tools"]); got != tools {
		t.Fatalf("tools = %s, want %s unchanged", got, tools)
	}
}

func TestABodyThatIsNotAJSONObjectIsARequestError(t *testing.T) {
	for _, body := range []string{`not json`, `["a"]`, ``, `null`} {
		err := rejection(t, body, false)
		if err.Code != "invalid_json" {
			t.Errorf("%q: Code = %q, want invalid_json", body, err.Code)
		}
	}
}
