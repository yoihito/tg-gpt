package adapters

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

func TestOpenAIResponseRequestWithToolsPreservesReasoning(t *testing.T) {
	req := toOpenAIResponseRequest(llm.Request{
		Model: "gpt-5.6-terra",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "search"},
		},
		Tools: []llm.Tool{
			{
				Name:        "web_search",
				Description: "Search the web",
				Parameters:  map[string]any{"type": "object"},
			},
		},
		ToolChoice: llm.ToolChoiceAuto,
	})

	if req.Model != "gpt-5.6-terra" {
		t.Fatalf("model: got %q", req.Model)
	}
	if req.Reasoning.Effort != shared.ReasoningEffortLow {
		t.Fatalf("reasoning effort: got %q, want low", req.Reasoning.Effort)
	}
	if len(req.Tools) != 1 || req.Tools[0].OfFunction == nil {
		t.Fatalf("tools were not converted: %+v", req.Tools)
	}
	if req.Tools[0].OfFunction.Name != "web_search" {
		t.Fatalf("tool name: got %q", req.Tools[0].OfFunction.Name)
	}
}

func TestOpenAIResponseInputMapsToolHistory(t *testing.T) {
	input := toOpenAIResponseInput([]llm.Message{
		{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "web_search", Arguments: `{"query":"test"}`},
			},
		},
		{
			Role: llm.RoleTool,
			ToolResult: &llm.ToolResult{
				CallID: "call_1",
				Name:   "web_search",
				Output: "",
			},
		},
	})

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got[0]["type"] != "function_call" || got[0]["call_id"] != "call_1" {
		t.Fatalf("function call item: %s", data)
	}
	if got[1]["type"] != "function_call_output" || got[1]["call_id"] != "call_1" || got[1]["output"] != " " {
		t.Fatalf("function output item: %s", data)
	}
}

func TestOpenAIResponseInputMapsVisionParts(t *testing.T) {
	input := toOpenAIResponseInput([]llm.Message{
		{
			Role: llm.RoleUser,
			Parts: []llm.ContentPart{
				{Type: llm.ContentPartText, Text: "what is this?"},
				{Type: llm.ContentPartImageURL, ImageURL: "data:image/jpeg;base64,abc"},
			},
		},
	})

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Content) != 2 {
		t.Fatalf("vision content: %s", data)
	}
	if got[0].Content[0]["type"] != "input_text" || got[0].Content[1]["type"] != "input_image" {
		t.Fatalf("vision content types: %s", data)
	}
}

func TestOpenAIResponseStreamOutputItemDoneFunctionCall(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"type":         "response.output_item.done",
		"output_index": 2,
		"item": map[string]any{
			"id":        "fc_test",
			"type":      "function_call",
			"call_id":   "call_1",
			"name":      "web_search",
			"arguments": `{"query":"test"}`,
			"status":    "completed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var event responses.ResponseStreamEventUnion
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}

	adapter := &OpenaiStreamAdapter{}
	converted, ok := adapter.fromResponseStreamEvent(event)
	if !ok || converted.ToolCall == nil {
		t.Fatalf("event was not converted: %+v", converted)
	}
	if converted.ToolCall.ID != "call_1" || converted.ToolCall.Index != 2 || converted.ToolCall.Name != "web_search" {
		t.Fatalf("tool call: %+v", converted.ToolCall)
	}
	if converted.ToolCall.Arguments != `{"query":"test"}` {
		t.Fatalf("tool call arguments: %+v", converted.ToolCall)
	}
}
