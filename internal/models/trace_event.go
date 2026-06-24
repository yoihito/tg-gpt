package models

import (
	"encoding/json"

	"github.com/sashabaranov/go-openai"
)

const (
	EventTypeUserMsg    = "user_msg"
	EventTypeModelMsg   = "model_msg"
	EventTypeToolCall   = "tool_call"
	EventTypeToolResult = "tool_result"
)

type TraceEvent struct {
	ID          int64
	UserID      int64
	DialogID    int64
	TurnIndex   int64
	EventType   string
	Payload     json.RawMessage
	TgMessageID *int64
	Model       string
	CreatedAt   int64
}

type UserMsgPayload struct {
	Content      string                   `json:"content"`
	MultiContent []openai.ChatMessagePart `json:"multi_content,omitempty"`
}

type ModelMsgPayload struct {
	Content   string            `json:"content"`
	ToolCalls []openai.ToolCall `json:"tool_calls,omitempty"`
}

type ToolResultPayload struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Result     string `json:"result"`
}
