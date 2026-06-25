package services

import (
	"encoding/json"
	"reflect"
	"testing"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

func TestRRFFuseAgreement(t *testing.T) {
	// Same ranking from both sources: top item should stay top.
	got := rrfFuse([]int64{10, 20, 30}, []int64{10, 20, 30}, 3)
	want := []int64{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("agreement: got %v want %v", got, want)
	}
}

func TestRRFFuseDisagreement(t *testing.T) {
	// Item 10 ranks #1 lexically but #20 in vector. Item 20 ranks #2 in both.
	// RRF for 20 (k=60): 1/62 + 1/62 = 0.0323
	// RRF for 10:        1/61 + 1/80 = 0.0288
	// So 20 should beat 10.
	got := rrfFuse(
		[]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100,
			11, 12, 13, 14, 15, 16, 17, 18, 19, 99}, // 10 is rank 1
		[]int64{99, 20, 88, 77, 66, 55, 44, 33, 22, 11,
			21, 22, 23, 24, 25, 26, 27, 28, 29, 10}, // 10 is rank 20
		2,
	)
	if got[0] != 99 && got[0] != 20 {
		t.Errorf("expected 99 or 20 to win; got %v", got)
	}
}

func TestRRFFuseEmpty(t *testing.T) {
	if got := rrfFuse(nil, nil, 5); len(got) != 0 {
		t.Errorf("empty inputs should return empty; got %v", got)
	}
}

func TestRRFFuseOneSide(t *testing.T) {
	got := rrfFuse([]int64{1, 2, 3}, nil, 5)
	want := []int64{1, 2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("one-sided: got %v want %v", got, want)
	}
}

func TestNormalizeContentCollapsesWhitespace(t *testing.T) {
	got := normalizeContent("  Hello\tworld\n\n  ")
	want := "hello world"
	if got != want {
		t.Errorf("normalize: got %q want %q", got, want)
	}
}

func TestContentHashStability(t *testing.T) {
	a := contentHash("Hello World")
	b := contentHash("hello   world")
	if a != b {
		t.Errorf("hash should be invariant to case/whitespace: %v vs %v", a, b)
	}
	c := contentHash("Hello Wo")
	if a == c {
		t.Errorf("different content should produce different hash")
	}
}

func TestAppendTraceMessagesSkipsIncompleteToolCallGroup(t *testing.T) {
	events := []models.TraceEvent{
		traceEvent(t, models.EventTypeUserMsg, models.UserMsgPayload{Content: "search this"}),
		traceEvent(t, models.EventTypeModelMsg, models.ModelMsgPayload{
			Content: "",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "web_search", Arguments: `{"query":"x"}`},
			},
		}),
		traceEvent(t, models.EventTypeUserMsg, models.UserMsgPayload{Content: "try again"}),
	}

	got := appendTraceMessages(nil, events)
	if len(got) != 2 {
		t.Fatalf("expected incomplete tool-call group to be skipped, got %#v", got)
	}
	if got[0].Role != llm.RoleUser || got[0].Content != "search this" {
		t.Fatalf("unexpected first message: %#v", got[0])
	}
	if got[1].Role != llm.RoleUser || got[1].Content != "try again" {
		t.Fatalf("unexpected second message: %#v", got[1])
	}
}

func TestAppendTraceMessagesKeepsCompleteToolCallGroup(t *testing.T) {
	events := []models.TraceEvent{
		traceEvent(t, models.EventTypeUserMsg, models.UserMsgPayload{Content: "search this"}),
		traceEvent(t, models.EventTypeModelMsg, models.ModelMsgPayload{
			Content: "",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "web_search", Arguments: `{"query":"x"}`},
			},
		}),
		traceEvent(t, models.EventTypeToolResult, models.ToolResultPayload{
			ToolCallID: "call_1",
			Name:       "web_search",
			Result:     "result",
		}),
	}

	got := appendTraceMessages(nil, events)
	if len(got) != 3 {
		t.Fatalf("expected complete tool-call group, got %#v", got)
	}
	if got[1].Role != llm.RoleAssistant || len(got[1].ToolCalls) != 1 {
		t.Fatalf("unexpected assistant message: %#v", got[1])
	}
	if got[2].Role != llm.RoleTool || got[2].ToolResult == nil || got[2].ToolResult.CallID != "call_1" {
		t.Fatalf("unexpected tool result: %#v", got[2])
	}
}

func traceEvent(t *testing.T, eventType string, payload any) models.TraceEvent {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return models.TraceEvent{EventType: eventType, Payload: data}
}
