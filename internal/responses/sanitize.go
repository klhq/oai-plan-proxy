// Package responses adapts Responses API requests to the limits of ChatGPT
// plan usage.
package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request is a sanitized request ready to forward.
type Request struct {
	// Body is the JSON body to send upstream.
	Body []byte
	// Dropped names the parameters removed because the plan route rejects
	// them, sorted.
	Dropped []string
	// Stream reports whether the caller asked for a stream. Upstream always
	// streams.
	Stream bool
}

// previewLimited lists, in sorted order, the parameters the plan route
// rejects according to the preview-limits page.
var previewLimited = []string{
	"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata",
	"moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier",
	"temperature", "top_logprobs", "top_p", "truncation", "user",
}

// Sanitize rewrites a caller's /v1/responses body for the plan route. It
// decodes only the top level and edits only the fields it must, so every
// other value reaches upstream byte for byte.
func Sanitize(body []byte, strict bool) (Request, error) {
	var f map[string]json.RawMessage
	if err := json.Unmarshal(body, &f); err != nil || f == nil {
		return Request{}, &Error{Code: "invalid_json", Message: "request body must be a JSON object"}
	}
	if v, ok := f["previous_response_id"]; ok && string(v) == "null" {
		delete(f, "previous_response_id")
	}
	if _, ok := f["previous_response_id"]; ok {
		return Request{}, &Error{
			Param:   "previous_response_id",
			Code:    "unsupported_parameter",
			Message: "ChatGPT plan usage stores no responses, so previous_response_id cannot work; send the full conversation in input",
		}
	}
	if err := checkTools(f["tools"]); err != nil {
		return Request{}, err
	}
	var dropped []string
	for _, name := range previewLimited {
		if _, ok := f[name]; ok {
			delete(f, name)
			dropped = append(dropped, name)
		}
	}
	if strict && len(dropped) > 0 {
		return Request{}, &Error{
			Param: dropped[0],
			Code:  "unsupported_parameter",
			Message: "ChatGPT plan usage does not accept these parameters: " + strings.Join(dropped, ", ") +
				"; remove them or run the proxy without --strict to drop them",
		}
	}
	if in, ok := f["input"]; ok {
		input, err := normalizeInput(in)
		if err != nil {
			return Request{}, err
		}
		f["input"] = input
	}
	stream := string(f["stream"]) == "true"
	f["store"] = json.RawMessage("false")
	f["stream"] = json.RawMessage("true")
	out, err := json.Marshal(f)
	if err != nil {
		return Request{}, fmt.Errorf("encode request: %w", err)
	}
	return Request{Body: out, Dropped: dropped, Stream: stream}, nil
}

// Error is a request the plan route cannot serve. It maps to a 400.
type Error struct {
	// Param is the offending request field.
	Param string
	// Code is a machine-readable reason.
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// normalizeInput wraps a string input in a one-item message array and turns
// system-role items into developer-role items, which the plan route
// rejects. Every other item passes through byte for byte.
func normalizeInput(in json.RawMessage) (json.RawMessage, error) {
	var text string
	if err := json.Unmarshal(in, &text); err == nil {
		return json.Marshal([]map[string]string{{"role": "user", "content": text}})
	}
	var items []json.RawMessage
	if err := json.Unmarshal(in, &items); err != nil {
		return in, nil
	}
	for i, raw := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || string(item["role"]) != `"system"` {
			continue
		}
		item["role"] = json.RawMessage(`"developer"`)
		b, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("encode input item %d: %w", i, err)
		}
		items[i] = b
	}
	return json.Marshal(items)
}

// hostedTools are tool types the plan route cannot run. Dropping them in
// silence would let a model answer as if it had searched or run code.
// "computer" and "computer_use_preview" are the tools types in the Responses
// API reference; "computer_use" is the tool_choice name, kept in case a
// client sends it.
var hostedTools = map[string]bool{
	"image_generation":     true,
	"file_search":          true,
	"code_interpreter":     true,
	"computer":             true,
	"computer_use_preview": true,
	"computer_use":         true,
	"mcp":                  true,
	"tool_search":          true,
}

// checkTools rejects the first hosted tool in tools. Other entries,
// including function, custom and web_search, pass untouched.
func checkTools(tools json.RawMessage) error {
	var entries []struct {
		Type string `json:"type"`
	}
	if tools == nil || json.Unmarshal(tools, &entries) != nil {
		return nil
	}
	for i, e := range entries {
		if hostedTools[e.Type] {
			return &Error{
				Param:   fmt.Sprintf("tools[%d]", i),
				Code:    "unsupported_tool",
				Message: fmt.Sprintf("tool type %q is not available with ChatGPT plan usage; remove it from tools", e.Type),
			}
		}
	}
	return nil
}
