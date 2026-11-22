package chat

import (
	"encoding/json"

	"github.com/klhq/oai-plan-proxy/internal/responses"
)

// Streamer turns Responses stream events into chat.completion.chunk bodies.
type Streamer struct {
	includeUsage bool
	id, model    string
	created      int64
	started      bool
	// calls maps a function_call item id to its tool_calls index.
	calls map[string]int
}

// NewStreamer returns a Streamer. includeUsage adds the usage chunk that
// stream_options.include_usage asks for.
func NewStreamer(includeUsage bool) *Streamer {
	return &Streamer{includeUsage: includeUsage, calls: map[string]int{}}
}

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chatUsage    `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []toolCallDelta `json:"tool_calls,omitempty"`
}

type toolCallDelta struct {
	Index    int           `json:"index"`
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function functionDelta `json:"function"`
}

type functionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// Chunks returns the chunks one event produces, often none.
func (s *Streamer) Chunks(ev responses.Event) []json.RawMessage {
	switch ev.Type {
	case "response.created", "response.in_progress":
		var e struct {
			Response response `json:"response"`
		}
		if json.Unmarshal(ev.Data, &e) == nil {
			s.id, s.model, s.created = e.Response.ID, e.Response.Model, e.Response.CreatedAt
		}
	case "response.output_text.delta":
		var e struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		return s.emit(delta{Content: e.Delta})
	case "response.output_item.added":
		var e struct {
			Item struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		if e.Item.Type != "function_call" {
			return nil
		}
		i := len(s.calls)
		s.calls[e.Item.ID] = i
		return s.emit(delta{ToolCalls: []toolCallDelta{{Index: i, ID: e.Item.CallID, Type: "function",
			Function: functionDelta{Name: e.Item.Name}}}})
	case "response.function_call_arguments.delta":
		var e struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		i, ok := s.calls[e.ItemID]
		if !ok {
			return nil
		}
		return s.emit(delta{ToolCalls: []toolCallDelta{{Index: i, Function: functionDelta{Arguments: e.Delta}}}})
	case "response.completed":
		var e struct {
			Response response `json:"response"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		finish := e.Response.finishReason()
		out := s.start()
		out = append(out, s.chunk(delta{}, &finish))
		if s.includeUsage {
			b, _ := json.Marshal(chunk{
				ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
				Choices: []chunkChoice{}, Usage: e.Response.Usage.chat(),
			})
			out = append(out, b)
		}
		return out
	}
	return nil
}

// emit returns the chunk for d, preceded by the role chunk when it is the
// first output.
func (s *Streamer) emit(d delta) []json.RawMessage {
	return append(s.start(), s.chunk(d, nil))
}

// start returns the role chunk the first time it is called.
func (s *Streamer) start() []json.RawMessage {
	if s.started {
		return nil
	}
	s.started = true
	return []json.RawMessage{s.chunk(delta{Role: "assistant"}, nil)}
}

func (s *Streamer) chunk(d delta, finish *string) json.RawMessage {
	b, _ := json.Marshal(chunk{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []chunkChoice{{Delta: d, FinishReason: finish}},
	})
	return b
}
