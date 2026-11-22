// Package chat translates Chat Completions requests and responses to and from
// the Responses API that ChatGPT plan usage serves.
package chat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

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
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// functionCallOutput is a Responses function_call_output input item.
type functionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type toolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// functionCall is a Responses function_call input item.
type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type part struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal,omitempty"`
}

type item struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
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
	if raw, ok := f["tools"]; ok {
		tools, err := flattenTools(raw)
		if err != nil {
			return Translation{}, err
		}
		f["tools"] = tools
	}
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(f["tool_choice"], &named) == nil && named.Type == "function" {
		f["tool_choice"], _ = json.Marshal(map[string]string{"type": "function", "name": named.Function.Name})
	}
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
	if effort, ok := f["reasoning_effort"]; ok {
		delete(f, "reasoning_effort")
		f["reasoning"], _ = json.Marshal(map[string]json.RawMessage{"effort": effort})
	}
	if raw, ok := f["response_format"]; ok {
		delete(f, "response_format")
		format, err := textFormat(raw)
		if err != nil {
			return Translation{}, err
		}
		if format != nil {
			f["text"], _ = json.Marshal(map[string]json.RawMessage{"format": format})
		}
	}
	input := []any{}
	for i, m := range msgs {
		param := fmt.Sprintf("messages[%d].content", i)
		content, err := readContent(m.Content)
		if err != nil {
			return Translation{}, &responses.Error{Param: param, Code: "invalid_value", Message: err.Error()}
		}
		if m.Role == "tool" {
			var out strings.Builder
			for _, c := range content {
				out.WriteString(c.Text)
			}
			input = append(input, functionCallOutput{Type: "function_call_output", CallID: m.ToolCallID, Output: out.String()})
			continue
		}
		role := m.Role
		if role == "system" {
			// The plan route rejects system items.
			role = "developer"
		}
		parts := []any{}
		for j, c := range content {
			p, err := c.responsesPart(role)
			if err != nil {
				return Translation{}, &responses.Error{Param: fmt.Sprintf("%s[%d]", param, j), Code: "unsupported_content", Message: err.Error()}
			}
			parts = append(parts, p)
		}
		if len(parts) > 0 || len(m.ToolCalls) == 0 {
			input = append(input, item{Role: role, Content: parts})
		}
		for _, c := range m.ToolCalls {
			input = append(input, functionCall{Type: "function_call", CallID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
		}
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

// contentPart is one part of Chat Completions message content.
type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	} `json:"image_url"`
	File struct {
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
		FileID   string `json:"file_id"`
	} `json:"file"`
}

// inputFile is a Responses input_file part.
type inputFile struct {
	Type     string `json:"type"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data"`
}

// refusalPart is a Responses refusal part in assistant history.
type refusalPart struct {
	Type    string `json:"type"`
	Refusal string `json:"refusal"`
}

// inputImage is a Responses input_image part.
type inputImage struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail,omitempty"`
}

// readContent reads message content, which is null, a string, or an array
// of typed parts.
func readContent(raw json.RawMessage) ([]contentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []contentPart{{Type: "text", Text: text}}, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	return parts, nil
}

// responsesPart converts c to the Responses part a message with role holds.
func (c contentPart) responsesPart(role string) (any, error) {
	switch c.Type {
	case "text":
		if role == "assistant" {
			return part{Type: "output_text", Text: c.Text}, nil
		}
		return part{Type: "input_text", Text: c.Text}, nil
	case "image_url":
		return inputImage{Type: "input_image", ImageURL: c.ImageURL.URL, Detail: c.ImageURL.Detail}, nil
	case "file":
		if c.File.FileData == "" {
			return nil, fmt.Errorf("a file part needs file_data: ChatGPT plan usage has no file upload, so file_id cannot be resolved")
		}
		return inputFile{Type: "input_file", Filename: c.File.Filename, FileData: c.File.FileData}, nil
	case "refusal":
		return refusalPart{Type: "refusal", Refusal: c.Refusal}, nil
	case "input_audio":
		return nil, fmt.Errorf("audio input is not supported with ChatGPT plan usage")
	}
	return nil, fmt.Errorf("content part type %q is not supported", c.Type)
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

// flattenTools moves each function tool's fields up one level, the shape the
// Responses API uses. Field values are kept byte for byte.
func flattenTools(raw json.RawMessage) (json.RawMessage, error) {
	var tools []struct {
		Type     string                     `json:"type"`
		Function map[string]json.RawMessage `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, &responses.Error{Param: "tools", Code: "invalid_value", Message: "tools must be an array of tools"}
	}
	out := make([]map[string]json.RawMessage, 0, len(tools))
	for i, t := range tools {
		if t.Type != "function" {
			return nil, &responses.Error{Param: fmt.Sprintf("tools[%d]", i), Code: "unsupported_tool",
				Message: fmt.Sprintf("tool type %q is not supported on /v1/chat/completions; use function tools, or call /v1/responses", t.Type)}
		}
		flat := map[string]json.RawMessage{"type": json.RawMessage(`"function"`)}
		for k, v := range t.Function {
			flat[k] = v
		}
		out = append(out, flat)
	}
	return json.Marshal(out)
}

// textFormat maps response_format to the Responses text.format value. It
// returns nil for the plain text format, which is the default.
func textFormat(raw json.RawMessage) (json.RawMessage, error) {
	var rf struct {
		Type       string                     `json:"type"`
		JSONSchema map[string]json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		return nil, &responses.Error{Param: "response_format", Code: "invalid_value", Message: "response_format must be an object"}
	}
	switch rf.Type {
	case "text":
		return nil, nil
	case "json_object":
		return json.RawMessage(`{"type":"json_object"}`), nil
	case "json_schema":
		format := map[string]json.RawMessage{"type": json.RawMessage(`"json_schema"`)}
		for k, v := range rf.JSONSchema {
			format[k] = v
		}
		return json.Marshal(format)
	}
	return nil, &responses.Error{Param: "response_format", Code: "invalid_value",
		Message: fmt.Sprintf("response_format type %q is not supported", rf.Type)}
}
