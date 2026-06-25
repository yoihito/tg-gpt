package llm

import (
	"context"
	"encoding/json"
)

type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderGemini    Provider = "gemini"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ContentPartType string

const (
	ContentPartText     ContentPartType = "text"
	ContentPartImageURL ContentPartType = "image_url"
)

type ContentPart struct {
	Type     ContentPartType `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL string          `json:"image_url,omitempty"`
}

func (p *ContentPart) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type     ContentPartType `json:"type"`
		Text     string          `json:"text,omitempty"`
		ImageURL json.RawMessage `json:"image_url,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Type = raw.Type
	p.Text = raw.Text
	if len(raw.ImageURL) > 0 {
		var url string
		if err := json.Unmarshal(raw.ImageURL, &url); err == nil {
			p.ImageURL = url
		} else {
			var obj struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(raw.ImageURL, &obj); err != nil {
				return err
			}
			p.ImageURL = obj.URL
		}
	}
	return nil
}

type Message struct {
	Role       Role          `json:"role"`
	Content    string        `json:"content,omitempty"`
	Parts      []ContentPart `json:"parts,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolResult *ToolResult   `json:"tool_result,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict,omitempty"`
}

type ToolChoice string

const (
	ToolChoiceAuto ToolChoice = "auto"
	ToolChoiceNone ToolChoice = "none"
)

type Request struct {
	Model      string
	Messages   []Message
	Tools      []Tool
	ToolChoice ToolChoice
}

type ToolCall struct {
	ID        string `json:"id"`
	Index     int    `json:"index,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (c *ToolCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        string `json:"id"`
		Index     *int   `json:"index,omitempty"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Function  *struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.ID = raw.ID
	if raw.Index != nil {
		c.Index = *raw.Index
	}
	c.Name = raw.Name
	c.Arguments = raw.Arguments
	if raw.Function != nil {
		if c.Name == "" {
			c.Name = raw.Function.Name
		}
		if c.Arguments == "" {
			c.Arguments = raw.Function.Arguments
		}
	}
	return nil
}

type ToolResult struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Output string `json:"output"`
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

type StreamEvent struct {
	TextDelta string
	ToolCall  *ToolCall
	ToolCalls []ToolCall
	Usage     *Usage
	Done      bool
}

type Capabilities struct {
	FunctionTools bool
	Vision        bool
	ToolChoice    bool
}

type Client interface {
	Provider() Provider
	Capabilities(model string) Capabilities
	Stream(ctx context.Context, req Request) (Stream, error)
}

type Stream interface {
	Next() bool
	Event() StreamEvent
	Err() error
	Close() error
}
