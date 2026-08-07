package services

import (
	"encoding/json"
	"reflect"
	"strings"
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

func TestAssemblePromptExcludesFactsAndEpisodesFromSystemMessage(t *testing.T) {
	r := &Retriever{}
	retrieved := RetrievedMemory{
		Preferences: []models.Preference{{PrefKey: "timezone", PrefValue: "Europe/Berlin"}},
		Facts:       []models.Fact{{Subject: "job", Content: "works as an engineer"}},
		Episodes:    []models.Episode{{Summary: "discussed vacation plans"}},
	}

	messages := r.AssemblePrompt("Base system prompt.", retrieved)
	if len(messages) != 1 {
		t.Fatalf("expected only the system message (no trace events), got %d: %#v", len(messages), messages)
	}
	sys := messages[0].Content
	if !strings.Contains(sys, "User preferences") {
		t.Fatalf("expected preferences in the system message: %q", sys)
	}
	if strings.Contains(sys, "Relevant facts") || strings.Contains(sys, "Relevant past episodes") {
		t.Fatalf("facts/episodes must not be baked into the system message — they're retrieved per-query and would invalidate prompt caching for every turn: %q", sys)
	}
}

func TestAppendRetrievalContextLeavesStablePrefixUntouched(t *testing.T) {
	r := &Retriever{}
	history := []llm.Message{{Role: llm.RoleSystem, Content: "stable prefix"}}

	if got := r.AppendRetrievalContext(history, RetrievedMemory{}); len(got) != 1 {
		t.Fatalf("expected no trailing message when nothing was retrieved, got %#v", got)
	}

	got := r.AppendRetrievalContext(history, RetrievedMemory{
		Facts:    []models.Fact{{Subject: "job", Content: "works as an engineer"}},
		Episodes: []models.Episode{{Summary: "discussed vacation plans"}},
	})
	if len(got) != 2 {
		t.Fatalf("expected exactly one trailing message, got %d: %#v", len(got), got)
	}
	if got[0].Content != "stable prefix" {
		t.Fatalf("the stable prefix must be untouched: %#v", got[0])
	}
	if !strings.Contains(got[1].Content, "Relevant facts") || !strings.Contains(got[1].Content, "Relevant past episodes") {
		t.Fatalf("trailing message should contain both sections: %q", got[1].Content)
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
