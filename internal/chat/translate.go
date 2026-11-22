// Package chat translates Chat Completions requests and responses to and from
// the Responses API that ChatGPT plan usage serves.
package chat

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// kept are the Chat Completions fields that the Responses request keeps, as
// is or translated. Fields the stage 1 sanitizer handles are kept too, so it
// drops and lists them in one place.
var kept = map[string]bool{
	"model": true, "service_tier": true, "parallel_tool_calls": true,
	"tools": true, "tool_choice": true, "response_format": true,
	"reasoning_effort": true, "store": true, "stream": true,
}

// Translation is a Chat Completions request rewritten as a Responses request.
type Translation struct {
	// Body is the Responses request body, before the stage 1 sanitizing.
	Body []byte
	// Dropped names the request fields removed because the Responses API has
	// no equivalent, sorted.
	Dropped []string
	// Stream and IncludeUsage record what the caller asked for. The
	// Responses request always streams.
	Stream, IncludeUsage bool
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type part struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal,omitempty"`
}

type item struct {
	Role    string `json:"role"`
	Content []part `json:"content"`
}

// Translate rewrites a Chat Completions request body as a Responses request.
func Translate(body []byte) (Translation, error) {
	var f map[string]json.RawMessage
	if err := json.Unmarshal(body, &f); err != nil || f == nil {
		return Translation{}, &responses.Error{Code: "invalid_json",
			Message: "the request body must be a JSON object"}
	}
	var msgs []message
	if err := json.Unmarshal(f["messages"], &msgs); err != nil || msgs == nil {
		return Translation{}, &responses.Error{Param: "messages", Code: "invalid_value",
			Message: "messages must be an array of chat messages"}
	}
	if err := checkUnsupported(f); err != nil {
		return Translation{}, err
	}
	delete(f, "messages")
	var tr Translation
	_ = json.Unmarshal(f["stream"], &tr.Stream)
	var opts struct {
		IncludeUsage bool `json:"include_usage"`
	}
	_ = json.Unmarshal(f["stream_options"], &opts)
	tr.IncludeUsage = tr.Stream && opts.IncludeUsage
	delete(f, "stream_options")
	var dropped []string
	for k := range f {
		if !kept[k] && !responses.IsPreviewLimited(k) {
			dropped = append(dropped, k)
			delete(f, k)
		}
	}
	slices.Sort(dropped)
	input := []item{}
	for i, m := range msgs {
		texts, err := contentTexts(m.Content)
		if err != nil {
			return Translation{}, &responses.Error{Param: fmt.Sprintf("messages[%d].content", i),
				Code: "invalid_value", Message: err.Error()}
		}
		role := m.Role
		if role == "system" {
			// The plan route rejects system items.
			role = "developer"
		}
		kind := "input_text"
		if role == "assistant" {
			kind = "output_text"
		}
		parts := []part{}
		for _, text := range texts {
			parts = append(parts, part{Type: kind, Text: text})
		}
		input = append(input, item{Role: role, Content: parts})
	}
	in, err := json.Marshal(input)
	if err != nil {
		return Translation{}, err
	}
	f["input"] = in
	tr.Dropped = dropped
	tr.Body, err = json.Marshal(f)
	return tr, err
}

// contentTexts reads message content, which is a string or an array of
// typed parts.
func contentTexts(raw json.RawMessage) ([]string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []string{text}, nil
	}
	var parts []part
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return texts, nil
}

// checkUnsupported rejects requests whose answer the plan cannot shape as
// asked. Dropping these would return a different result in silence.
func checkUnsupported(f map[string]json.RawMessage) error {
	reject := func(param, why string) error {
		return &responses.Error{Param: param, Code: "unsupported_parameter",
			Message: param + " is not supported with ChatGPT plan usage: " + why}
	}
	for _, name := range []string{"functions", "function_call"} {
		if _, ok := f[name]; ok {
			return reject(name, "use tools and tool_choice instead")
		}
	}
	for _, name := range []string{"audio", "top_logprobs"} {
		if _, ok := f[name]; ok {
			return reject(name, "the plan route returns neither audio nor logprobs")
		}
	}
	var n int
	if json.Unmarshal(f["n"], &n) == nil && n > 1 {
		return reject("n", "the plan route returns one choice")
	}
	var logprobs bool
	if json.Unmarshal(f["logprobs"], &logprobs) == nil && logprobs {
		return reject("logprobs", "the plan route returns no logprobs")
	}
	var modalities []string
	if json.Unmarshal(f["modalities"], &modalities) == nil && slices.ContainsFunc(modalities, func(m string) bool { return m != "text" }) {
		return reject("modalities", "the plan route returns text only")
	}
	return nil
}
