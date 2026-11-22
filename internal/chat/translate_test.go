package chat_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/chat"
	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// fields decodes a translated body into its top-level raw fields.
func fields(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("translated body is not a JSON object: %v: %s", err, body)
	}
	return m
}

func translate(t *testing.T, body string) chat.Translation {
	t.Helper()
	tr, err := chat.Translate([]byte(body))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	return tr
}

// assertJSON compares raw JSON by value.
func assertJSON(t *testing.T, name string, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s is not JSON: %s", name, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want for %s: %v", name, err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("%s = %s\nwant %s", name, gb, wb)
	}
}

func TestAUserStringBecomesAnInputTextMessage(t *testing.T) {
	tr := translate(t, `{"model":"gpt-a","messages":[{"role":"user","content":"hi"}]}`)

	f := fields(t, tr.Body)
	assertJSON(t, "model", f["model"], `"gpt-a"`)
	assertJSON(t, "input", f["input"], `[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]`)
	if _, ok := f["messages"]; ok {
		t.Errorf("messages was kept in the Responses body")
	}
}

func TestSystemAndDeveloperMessagesBecomeDeveloperMessagesInPlace(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hi"},
		{"role":"developer","content":"use French"}]}`)

	assertJSON(t, "input", fields(t, tr.Body)["input"], `[
		{"role":"developer","content":[{"type":"input_text","text":"be brief"}]},
		{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"role":"developer","content":[{"type":"input_text","text":"use French"}]}]`)
}

func TestAssistantTextBecomesOutputText(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"}]}`)

	assertJSON(t, "input", fields(t, tr.Body)["input"], `[
		{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"output_text","text":"hello"}]}]`)
}

func TestArrayContentKeepsEachTextPart(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}]}`)

	assertJSON(t, "input", fields(t, tr.Body)["input"], `[
		{"role":"user","content":[{"type":"input_text","text":"one"},{"type":"input_text","text":"two"}]}]`)
}

func TestFieldsWithoutAResponsesEquivalentAreDroppedAndListed(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"max_tokens":10,"max_completion_tokens":20,"stop":["x"],"seed":1,"future_knob":true,
		"temperature":0.2,"service_tier":"auto","parallel_tool_calls":false}`)

	f := fields(t, tr.Body)
	for _, k := range []string{"max_tokens", "max_completion_tokens", "stop", "seed", "future_knob"} {
		if _, ok := f[k]; ok {
			t.Errorf("%s was kept, want it dropped", k)
		}
	}
	// temperature is left for the stage 1 sanitizer, which drops and lists it.
	for _, k := range []string{"model", "temperature", "service_tier", "parallel_tool_calls"} {
		if _, ok := f[k]; !ok {
			t.Errorf("%s was dropped, want it kept", k)
		}
	}
	want := []string{"future_knob", "max_completion_tokens", "max_tokens", "seed", "stop"}
	if !slices.Equal(tr.Dropped, want) {
		t.Fatalf("Dropped = %v, want %v", tr.Dropped, want)
	}
}

func TestRequestsTheProxyCannotHonorAreRejectedNamingTheField(t *testing.T) {
	const msgs = `"model":"m","messages":[{"role":"user","content":"hi"}]`
	for field, value := range map[string]string{
		"n":             `2`,
		"audio":         `{"voice":"alloy","format":"wav"}`,
		"modalities":    `["text","audio"]`,
		"logprobs":      `true`,
		"top_logprobs":  `2`,
		"functions":     `[{"name":"f"}]`,
		"function_call": `"auto"`,
	} {
		_, err := chat.Translate([]byte(`{` + msgs + `,"` + field + `":` + value + `}`))

		var reqErr *responses.Error
		if !errors.As(err, &reqErr) || reqErr.Param != field {
			t.Errorf("%s: err = %v, want a request error with param %q", field, err, field)
		}
	}
}

func TestHonorableValuesOfThoseFieldsAreAccepted(t *testing.T) {
	translate(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1,"modalities":["text"],"logprobs":false}`)
}

func TestABodyWithoutAMessagesArrayIsARequestError(t *testing.T) {
	for _, body := range []string{`not json`, `null`, `[]`, `{"model":"m"}`, `{"model":"m","messages":"hi"}`, `{"model":"m","messages":null}`} {
		_, err := chat.Translate([]byte(body))

		var reqErr *responses.Error
		if !errors.As(err, &reqErr) {
			t.Errorf("%s: err = %v, want a request error", body, err)
		}
	}
}

func TestMessageContentOfTheWrongTypeIsARequestError(t *testing.T) {
	_, err := chat.Translate([]byte(`{"model":"m","messages":[{"role":"user","content":5}]}`))

	var reqErr *responses.Error
	if !errors.As(err, &reqErr) || reqErr.Param != "messages[0].content" {
		t.Fatalf("err = %v, want a request error with param messages[0].content", err)
	}
}

func TestStreamAndIncludeUsageAreRecordedAndStreamOptionsIsRemoved(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)

	if !tr.Stream || !tr.IncludeUsage {
		t.Fatalf("Stream=%v IncludeUsage=%v, want both true", tr.Stream, tr.IncludeUsage)
	}
	if _, ok := fields(t, tr.Body)["stream_options"]; ok {
		t.Errorf("stream_options was kept; the Responses API has no include_usage")
	}
	if plain := translate(t, `{"model":"m","messages":[]}`); plain.Stream || plain.IncludeUsage {
		t.Errorf("without stream: Stream=%v IncludeUsage=%v, want both false", plain.Stream, plain.IncludeUsage)
	}
}

func TestFunctionToolsAreFlattened(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"function","function":{"name":"get_weather","description":"Weather now","parameters":{"type":"object","properties":{"city":{"type":"string"}}},"strict":true}}]}`)

	assertJSON(t, "tools", fields(t, tr.Body)["tools"], `[
		{"type":"function","name":"get_weather","description":"Weather now","parameters":{"type":"object","properties":{"city":{"type":"string"}}},"strict":true}]`)
}

func TestANonFunctionToolIsRejectedNamingItsIndex(t *testing.T) {
	_, err := chat.Translate([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"function","function":{"name":"f"}},{"type":"custom","custom":{"name":"g"}}]}`))

	var reqErr *responses.Error
	if !errors.As(err, &reqErr) || reqErr.Param != "tools[1]" {
		t.Fatalf("err = %v, want a request error with param tools[1]", err)
	}
}

func TestToolChoiceNamingAFunctionIsFlattenedAndStringsPassThrough(t *testing.T) {
	const msgs = `"model":"m","messages":[{"role":"user","content":"hi"}]`
	for choice, want := range map[string]string{
		`{"type":"function","function":{"name":"get_weather"}}`: `{"type":"function","name":"get_weather"}`,
		`"auto"`:     `"auto"`,
		`"none"`:     `"none"`,
		`"required"`: `"required"`,
	} {
		tr := translate(t, `{`+msgs+`,"tool_choice":`+choice+`}`)

		assertJSON(t, "tool_choice "+choice, fields(t, tr.Body)["tool_choice"], want)
	}
}

func TestAssistantToolCallsBecomeFunctionCallItemsAfterItsText(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"Checking.","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Taipei\"}"}}]},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]}]}`)

	assertJSON(t, "input", fields(t, tr.Body)["input"], `[
		{"role":"user","content":[{"type":"input_text","text":"weather?"}]},
		{"role":"assistant","content":[{"type":"output_text","text":"Checking."}]},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Taipei\"}"},
		{"type":"function_call","call_id":"call_2","name":"get_time","arguments":"{}"}]`)
}

func TestAToolMessageBecomesAFunctionCallOutput(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[
		{"role":"tool","tool_call_id":"call_1","content":"{\"temp\":21}"},
		{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"12:00"},{"type":"text","text":" UTC"}]}]}`)

	assertJSON(t, "input", fields(t, tr.Body)["input"], `[
		{"type":"function_call_output","call_id":"call_1","output":"{\"temp\":21}"},
		{"type":"function_call_output","call_id":"call_2","output":"12:00 UTC"}]`)
}

func TestAnImageURLPartBecomesAnInputImage(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"what is this?"},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"low"}},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`)

	assertJSON(t, "input", fields(t, tr.Body)["input"], `[{"role":"user","content":[
		{"type":"input_text","text":"what is this?"},
		{"type":"input_image","image_url":"https://example.com/cat.png","detail":"low"},
		{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]`)
}

func TestAFilePartWithInlineDataBecomesAnInputFile(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"file","file":{"filename":"notes.pdf","file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`)

	assertJSON(t, "input", fields(t, tr.Body)["input"], `[{"role":"user","content":[
		{"type":"input_file","filename":"notes.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]}]`)
}

func TestContentThePlanCannotTakeIsRejectedNamingThePart(t *testing.T) {
	for name, p := range map[string]string{
		"file_id":     `{"type":"file","file":{"file_id":"file-abc"}}`,
		"input_audio": `{"type":"input_audio","input_audio":{"data":"UklGRg==","format":"wav"}}`,
		"unknown":     `{"type":"hologram","hologram":{}}`,
	} {
		_, err := chat.Translate([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},` + p + `]}]}`))

		var reqErr *responses.Error
		if !errors.As(err, &reqErr) || reqErr.Param != "messages[0].content[1]" {
			t.Errorf("%s: err = %v, want a request error with param messages[0].content[1]", name, err)
		}
	}
}

func TestAnAssistantRefusalPartIsKeptAsARefusal(t *testing.T) {
	tr := translate(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"refusal","refusal":"No."}]}]}`)

	assertJSON(t, "input", fields(t, tr.Body)["input"], `[{"role":"assistant","content":[{"type":"refusal","refusal":"No."}]}]`)
}

func TestResponseFormatBecomesTextFormat(t *testing.T) {
	const msgs = `"model":"m","messages":[{"role":"user","content":"hi"}]`
	schema := `{"type":"json_schema","json_schema":{"name":"person","schema":{"type":"object","properties":{"name":{"type":"string"}}},"strict":true}}`
	for format, want := range map[string]string{
		schema:                   `{"format":{"type":"json_schema","name":"person","schema":{"type":"object","properties":{"name":{"type":"string"}}},"strict":true}}`,
		`{"type":"json_object"}`: `{"format":{"type":"json_object"}}`,
	} {
		f := fields(t, translate(t, `{`+msgs+`,"response_format":`+format+`}`).Body)

		assertJSON(t, "text for "+format, f["text"], want)
		if _, ok := f["response_format"]; ok {
			t.Errorf("%s: response_format was kept", format)
		}
	}
	if f := fields(t, translate(t, `{`+msgs+`,"response_format":{"type":"text"}}`).Body); f["text"] != nil || f["response_format"] != nil {
		t.Errorf("type text: text=%s response_format=%s, want both removed", f["text"], f["response_format"])
	}
}

func TestReasoningEffortBecomesReasoningEffort(t *testing.T) {
	f := fields(t, translate(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`).Body)

	assertJSON(t, "reasoning", f["reasoning"], `{"effort":"high"}`)
	if _, ok := f["reasoning_effort"]; ok {
		t.Errorf("reasoning_effort was kept")
	}
}

func TestAnUnknownResponseFormatIsARequestError(t *testing.T) {
	_, err := chat.Translate([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"yaml"}}`))

	var reqErr *responses.Error
	if !errors.As(err, &reqErr) || reqErr.Param != "response_format" {
		t.Fatalf("err = %v, want a request error with param response_format", err)
	}
}
