package adapters

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

func TestDeepInfraChatParamsIncludesToolsAndUsage(t *testing.T) {
	params := toDeepInfraChatParams(llm.Request{
		Model: "zai-org/GLM-5.2",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "be terse"},
			{Role: llm.RoleUser, Content: "search"},
		},
		Tools: []llm.Tool{
			{Name: "web_search", Description: "Search the web", Parameters: map[string]any{"type": "object"}},
		},
		ToolChoice: llm.ToolChoiceAuto,
	})

	if string(params.Model) != "zai-org/GLM-5.2" {
		t.Fatalf("model: got %q", params.Model)
	}
	if len(params.Messages) != 2 {
		t.Fatalf("messages: got %d, want 2", len(params.Messages))
	}
	if len(params.Tools) != 1 || params.Tools[0].OfFunction == nil {
		t.Fatalf("tools were not converted: %+v", params.Tools)
	}
	if params.Tools[0].OfFunction.Function.Name != "web_search" {
		t.Fatalf("tool name: got %q", params.Tools[0].OfFunction.Function.Name)
	}
	if !params.StreamOptions.IncludeUsage.Value {
		t.Fatalf("expected stream_options.include_usage to be true")
	}
	if !params.ParallelToolCalls.Valid() || params.ParallelToolCalls.Value {
		t.Fatalf("expected parallel_tool_calls to be explicitly false when tools are present")
	}
}

func TestDeepInfraMessagesMapsToolHistory(t *testing.T) {
	out := toDeepInfraMessages([]llm.Message{
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
				Output: "result text",
			},
		},
	})

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got[0]["role"] != "assistant" {
		t.Fatalf("assistant message: %s", data)
	}
	toolCalls, _ := got[0]["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got: %s", data)
	}
	if got[1]["role"] != "tool" || got[1]["tool_call_id"] != "call_1" || got[1]["content"] != "result text" {
		t.Fatalf("tool result message: %s", data)
	}
}

func TestDeepInfraMessagesMapsVisionParts(t *testing.T) {
	out := toDeepInfraMessages([]llm.Message{
		{
			Role: llm.RoleUser,
			Parts: []llm.ContentPart{
				{Type: llm.ContentPartText, Text: "what is this?"},
				{Type: llm.ContentPartImageURL, ImageURL: "data:image/jpeg;base64,abc"},
			},
		},
	})

	data, err := json.Marshal(out)
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
	if got[0].Content[0]["type"] != "text" || got[0].Content[1]["type"] != "image_url" {
		t.Fatalf("vision content types: %s", data)
	}
}

func TestFromChatCompletionChunkTextDelta(t *testing.T) {
	var chunk openai.ChatCompletionChunk
	if err := json.Unmarshal([]byte(`{
		"id": "c1", "model": "m", "object": "chat.completion.chunk", "created": 0,
		"choices": [{"index": 0, "delta": {"content": "hi"}, "finish_reason": ""}]
	}`), &chunk); err != nil {
		t.Fatal(err)
	}
	events := fromChatCompletionChunk(chunk)
	if len(events) != 1 || events[0].TextDelta != "hi" {
		t.Fatalf("events: %+v", events)
	}
}

func TestFromChatCompletionChunkToolCallDelta(t *testing.T) {
	var chunk openai.ChatCompletionChunk
	if err := json.Unmarshal([]byte(`{
		"id": "c1", "model": "m", "object": "chat.completion.chunk", "created": 0,
		"choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": {"name": "web_search", "arguments": "{\"query\":"}}]}, "finish_reason": ""}]
	}`), &chunk); err != nil {
		t.Fatal(err)
	}
	events := fromChatCompletionChunk(chunk)
	if len(events) != 1 || events[0].ToolCall == nil {
		t.Fatalf("events: %+v", events)
	}
	tc := events[0].ToolCall
	if tc.ID != "call_1" || tc.Index != 0 || tc.Name != "web_search" || tc.Arguments != `{"query":` {
		t.Fatalf("tool call: %+v", tc)
	}
}

func TestFromChatCompletionChunkUsage(t *testing.T) {
	var chunk openai.ChatCompletionChunk
	if err := json.Unmarshal([]byte(`{
		"id": "c1", "model": "m", "object": "chat.completion.chunk", "created": 0,
		"choices": [],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "prompt_tokens_details": {"cached_tokens": 3}}
	}`), &chunk); err != nil {
		t.Fatal(err)
	}
	events := fromChatCompletionChunk(chunk)
	if len(events) != 1 || events[0].Usage == nil {
		t.Fatalf("events: %+v", events)
	}
	usage := events[0].Usage
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.CachedInputTokens != 3 {
		t.Fatalf("usage: %+v", usage)
	}
	if !events[0].Done {
		t.Fatalf("expected Done to be true on the usage event")
	}
}

// fakeChunkStream implements chatCompletionChunkStream for testing DeepInfraStreamAdapter
// without a live HTTP connection.
type fakeChunkStream struct {
	chunks []openai.ChatCompletionChunk
	idx    int
}

func (f *fakeChunkStream) Next() bool {
	if f.idx >= len(f.chunks) {
		return false
	}
	f.idx++
	return true
}
func (f *fakeChunkStream) Current() openai.ChatCompletionChunk { return f.chunks[f.idx-1] }
func (f *fakeChunkStream) Err() error                          { return nil }
func (f *fakeChunkStream) Close() error                        { return nil }

func TestDeepInfraStreamAdapterDrainsMultipleEventsPerChunk(t *testing.T) {
	var chunk openai.ChatCompletionChunk
	if err := json.Unmarshal([]byte(`{
		"id": "c1", "model": "m", "object": "chat.completion.chunk", "created": 0,
		"choices": [{"index": 0, "delta": {"content": "hi", "tool_calls": [{"index": 0, "function": {"arguments": "x"}}]}, "finish_reason": ""}]
	}`), &chunk); err != nil {
		t.Fatal(err)
	}
	adapter := &DeepInfraStreamAdapter{stream: &fakeChunkStream{chunks: []openai.ChatCompletionChunk{chunk}}}

	if !adapter.Next() {
		t.Fatal("expected first event")
	}
	if adapter.Event().TextDelta != "hi" {
		t.Fatalf("first event: %+v", adapter.Event())
	}
	if !adapter.Next() {
		t.Fatal("expected second event")
	}
	if adapter.Event().ToolCall == nil || adapter.Event().ToolCall.Arguments != "x" {
		t.Fatalf("second event: %+v", adapter.Event())
	}
	if adapter.Next() {
		t.Fatalf("expected stream to end, got: %+v", adapter.Event())
	}
}
