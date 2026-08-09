package repositories

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

// newTestTraceRepo also registers a user with the given id, since trace_events.user_id
// carries a foreign key to users(id).
func newTestTraceRepo(t *testing.T, userID int64) *TraceRepo {
	t.Helper()
	db, err := database.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := NewUserRepo(db).Register(userID, "Test", "", "test", userID, true, "test-model"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	return NewTraceRepo(db)
}

func mustAppendTraceEvent(t *testing.T, repo *TraceRepo, userID, dialogID int64, eventType string, payload any) {
	t.Helper()
	if _, err := repo.Append(AppendEventInput{
		UserID:    userID,
		DialogID:  dialogID,
		EventType: eventType,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("append %s: %v", eventType, err)
	}
}

// TestGetRecentTurnsBoundsByTurnNotEventCount is the regression test for the "bot loses
// context of the previous message" bug: the old GetRecent capped the window by raw event
// count, so a tool-heavy turn (one model_msg per round plus one tool_result per call)
// could push the previous turn's exchange out of the window entirely, or leave only a
// partial tail of it. GetRecentTurns must return whole turns instead.
func TestGetRecentTurnsBoundsByTurnNotEventCount(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	// Turn 1: three tool calls in one round — 6 events by itself, which alone would have
	// exceeded the old default window of 8 combined with any other turn.
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn1 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{
		Content: "",
		ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "save_memory"},
			{ID: "c2", Name: "web_search"},
			{ID: "c3", Name: "save_fact"},
		},
	})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeToolResult, models.ToolResultPayload{ToolCallID: "c1"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeToolResult, models.ToolResultPayload{ToolCallID: "c2"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeToolResult, models.ToolResultPayload{ToolCallID: "c3"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn1 answer"})

	// Turn 2: the message currently being answered.
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn2 user"})

	got, err := repo.GetRecentTurns(userID, dialogID, 2)
	if err != nil {
		t.Fatalf("GetRecentTurns: %v", err)
	}
	if len(got) != 7 {
		t.Fatalf("expected all 6 events from turn1 plus turn2's user_msg (7 total), got %d: %#v", len(got), got)
	}
	if got[0].EventType != models.EventTypeUserMsg {
		t.Fatalf("window must start on turn1's user_msg, got %s", got[0].EventType)
	}
	var lastPayload models.UserMsgPayload
	if err := json.Unmarshal(got[0].Payload, &lastPayload); err != nil {
		t.Fatalf("unmarshal first event: %v", err)
	}
	if lastPayload.Content != "turn1 user" {
		t.Fatalf("expected window to start at turn1, got %q", lastPayload.Content)
	}
}

// TestGetRecentTurnsExcludesOlderTurns confirms the window is still bounded — it must not
// silently return the entire dialog history when older turns exist beyond numTurns.
func TestGetRecentTurnsExcludesOlderTurns(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn1 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn1 answer"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn2 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn2 answer"})

	got, err := repo.GetRecentTurns(userID, dialogID, 1)
	if err != nil {
		t.Fatalf("GetRecentTurns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected only turn2's 2 events, got %d: %#v", len(got), got)
	}
	var payload models.UserMsgPayload
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal first event: %v", err)
	}
	if payload.Content != "turn2 user" {
		t.Fatalf("expected window to start at turn2, got %q", payload.Content)
	}
}

// TestGetRecentTurnsReturnsWholeDialogWhenFewerTurnsExist covers the case where the
// dialog hasn't accumulated numTurns turns yet.
func TestGetRecentTurnsReturnsWholeDialogWhenFewerTurnsExist(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "only user msg"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "only answer"})

	got, err := repo.GetRecentTurns(userID, dialogID, 5)
	if err != nil {
		t.Fatalf("GetRecentTurns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both events for a dialog with fewer turns than requested, got %d", len(got))
	}
}
