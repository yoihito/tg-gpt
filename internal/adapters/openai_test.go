package adapters

import (
	"encoding/json"
	"testing"

	"vadimgribanov.com/tg-gpt/internal/llm"
)

func TestOpenAIAssistantToolCallMessageHasStringContent(t *testing.T) {
	messages := toOpenAIChatMessages([]llm.Message{
		{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "web_search", Arguments: `{"query":"test"}`},
			},
		},
	})
	data, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["content"].(string); !ok {
		t.Fatalf("content must serialize as string, got %s", data)
	}
}

func TestOpenAIToolResultMessageHasStringContent(t *testing.T) {
	messages := toOpenAIChatMessages([]llm.Message{
		{
			Role: llm.RoleTool,
			ToolResult: &llm.ToolResult{
				CallID: "call_1",
				Name:   "web_search",
				Output: "",
			},
		},
	})
	data, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["content"].(string); !ok {
		t.Fatalf("content must serialize as string, got %s", data)
	}
}
