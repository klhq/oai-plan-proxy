package chat_test

import (
	"encoding/json"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/chat"
)

// completedResponse is a trimmed response object from response.completed.
const completedResponse = `{
	"id":"resp_1","object":"response","created_at":1763800000,"model":"gpt-a","status":"completed",
	"output":[
		{"type":"message","id":"msg_1","role":"assistant","content":[
			{"type":"output_text","text":"Hello","annotations":[]},
			{"type":"output_text","text":" there","annotations":[]}]}],
	"usage":null}`

func completion(t *testing.T, response string) []byte {
	t.Helper()
	b, err := chat.Completion([]byte(response))
	if err != nil {
		t.Fatalf("Completion: %v", err)
	}
	return b
}

func TestACompletedResponseBecomesAChatCompletion(t *testing.T) {
	got := completion(t, completedResponse)

	assertJSON(t, "completion", got, `{
		"id":"resp_1","object":"chat.completion","created":1763800000,"model":"gpt-a",
		"choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"Hello there","refusal":null}}]}`)
}

func TestUsageIsMappedToChatNames(t *testing.T) {
	got := completion(t, `{"id":"r","output":[],"usage":{
		"input_tokens":20,"input_tokens_details":{"cached_tokens":4},
		"output_tokens":11,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":31}}`)

	assertJSON(t, "usage", fields(t, got)["usage"], `{
		"prompt_tokens":20,"completion_tokens":11,"total_tokens":31,
		"prompt_tokens_details":{"cached_tokens":4},
		"completion_tokens_details":{"reasoning_tokens":3}}`)
}

func TestAnIncompleteResponseFinishesWithItsReason(t *testing.T) {
	for reason, want := range map[string]string{
		"max_output_tokens": `"length"`,
		"content_filter":    `"content_filter"`,
	} {
		got := completion(t, `{"id":"r","status":"incomplete","incomplete_details":{"reason":"`+reason+`"},"output":[]}`)

		var c struct {
			Choices []struct {
				FinishReason json.RawMessage `json:"finish_reason"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(got, &c)
		if len(c.Choices) != 1 || string(c.Choices[0].FinishReason) != want {
			t.Errorf("%s: %s, want finish_reason %s", reason, got, want)
		}
	}
}

func TestARefusalFillsRefusalAndLeavesContentNull(t *testing.T) {
	got := completion(t, `{"id":"r","output":[{"type":"message","role":"assistant","content":[
		{"type":"refusal","refusal":"I can't help with that."}]}]}`)

	var c struct {
		Choices []struct {
			Message map[string]json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(got, &c)
	m := c.Choices[0].Message
	if string(m["content"]) != "null" || string(m["refusal"]) != `"I can't help with that."` {
		t.Fatalf("message = content %s refusal %s, want null and the refusal text", m["content"], m["refusal"])
	}
}
