package chat_test

import (
	"encoding/json"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/chat"
	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// event builds a Responses stream event from its JSON data.
func event(data string) responses.Event {
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(data), &head)
	return responses.Event{Data: []byte(data), Type: head.Type}
}

const created = `{"type":"response.created","response":{"id":"resp_1","created_at":1763800000,"model":"gpt-a","status":"in_progress","output":[]}}`

// feed runs events through a new streamer and collects every chunk.
func feed(t *testing.T, includeUsage bool, events ...string) []json.RawMessage {
	t.Helper()
	s := chat.NewStreamer(includeUsage)
	var chunks []json.RawMessage
	for _, e := range events {
		for _, c := range s.Chunks(event(e)) {
			chunks = append(chunks, c)
		}
	}
	return chunks
}

func TestTextDeltasStreamAsARoleChunkThenContentChunks(t *testing.T) {
	chunks := feed(t, false, created,
		`{"type":"response.output_text.delta","delta":"Hel"}`,
		`{"type":"response.output_text.delta","delta":"lo"}`)

	if len(chunks) != 3 {
		t.Fatalf("got %d chunks %s, want a role chunk and two content chunks", len(chunks), chunks)
	}
	head := `"id":"resp_1","object":"chat.completion.chunk","created":1763800000,"model":"gpt-a"`
	assertJSON(t, "chunk 0", chunks[0], `{`+head+`,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
	assertJSON(t, "chunk 1", chunks[1], `{`+head+`,"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`)
	assertJSON(t, "chunk 2", chunks[2], `{`+head+`,"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`)
}

const completedWithUsage = `{"type":"response.completed","response":{"id":"resp_1","created_at":1763800000,"model":"gpt-a","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`

func TestCompletionEndsWithAFinishChunkAndAUsageChunkWhenAsked(t *testing.T) {
	text := `{"type":"response.output_text.delta","delta":"Hi"}`

	plain := feed(t, false, created, text, completedWithUsage)
	withUsage := feed(t, true, created, text, completedWithUsage)

	head := `"id":"resp_1","object":"chat.completion.chunk","created":1763800000,"model":"gpt-a"`
	finish := `{` + head + `,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	if len(plain) != 3 {
		t.Fatalf("without include_usage: %d chunks %s, want role, content, finish", len(plain), plain)
	}
	assertJSON(t, "finish chunk", plain[2], finish)
	if len(withUsage) != 4 {
		t.Fatalf("with include_usage: %d chunks %s, want role, content, finish, usage", len(withUsage), withUsage)
	}
	assertJSON(t, "usage chunk", withUsage[3], `{`+head+`,"choices":[],
		"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,
			"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`)
}
