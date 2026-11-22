package chat

import (
	"encoding/json"
	"strings"
)

// response is the part of a Responses response object the translation reads.
type response struct {
	ID         string       `json:"id"`
	CreatedAt  int64        `json:"created_at"`
	Model      string       `json:"model"`
	Output     []outputItem `json:"output"`
	Usage      *usage       `json:"usage"`
	Status     string       `json:"status"`
	Incomplete *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// finishReason maps how a response ended to a Chat Completions
// finish_reason.
func (r *response) finishReason() string {
	if r.Status == "incomplete" && r.Incomplete != nil {
		switch r.Incomplete.Reason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		}
	}
	return "stop"
}

type usage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// chatUsage maps Responses usage to Chat Completions names.
func (u *usage) chat() *chatUsage {
	if u == nil {
		return nil
	}
	c := &chatUsage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
	c.PromptTokensDetails.CachedTokens = u.InputTokensDetails.CachedTokens
	c.CompletionTokensDetails.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	return c
}

type outputItem struct {
	Type    string `json:"type"`
	Content []part `json:"content"`
}

type chatCompletion struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Created int64      `json:"created"`
	Model   string     `json:"model"`
	Choices []choice   `json:"choices"`
	Usage   *chatUsage `json:"usage,omitempty"`
}

type choice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatMessage struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
	Refusal *string `json:"refusal"`
}

// Completion turns the response object of a response.completed event into a
// chat.completion body.
func Completion(body []byte) ([]byte, error) {
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	var text, refusal strings.Builder
	var hasText, hasRefusal bool
	for _, it := range r.Output {
		if it.Type != "message" {
			continue
		}
		for _, p := range it.Content {
			switch p.Type {
			case "output_text":
				text.WriteString(p.Text)
				hasText = true
			case "refusal":
				refusal.WriteString(p.Refusal)
				hasRefusal = true
			}
		}
	}
	msg := chatMessage{Role: "assistant"}
	if hasText {
		msg.Content = new(text.String())
	}
	if hasRefusal {
		msg.Refusal = new(refusal.String())
	}
	return json.Marshal(chatCompletion{
		ID: r.ID, Object: "chat.completion", Created: r.CreatedAt, Model: r.Model,
		Choices: []choice{{
			Message:      msg,
			FinishReason: r.finishReason(),
		}},
		Usage: r.Usage.chat(),
	})
}
