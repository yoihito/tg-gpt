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

// TestFindNthUserMsgTurnIndexLocatesWholeTurnBoundary is the regression test for the
// "bot loses context of the previous message" bug: an earlier version of this window
// capped by raw event count, so a tool-heavy turn (one model_msg per round plus one
// tool_result per call) could push the previous turn's exchange out of the window
// entirely, or leave only a partial tail of it. The 0th user_msg at-or-after a given
// point must still land exactly on a turn boundary regardless of how many events that
// turn contains.
func TestFindNthUserMsgTurnIndexLocatesWholeTurnBoundary(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	// Turn 1: three tool calls in one round — 6 events by itself.
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

	turnIndex, ok, err := repo.FindNthUserMsgTurnIndex(userID, dialogID, 0, 1)
	if err != nil {
		t.Fatalf("FindNthUserMsgTurnIndex: %v", err)
	}
	if !ok {
		t.Fatal("expected a second turn to exist")
	}

	got, err := repo.GetEventsFrom(userID, dialogID, turnIndex)
	if err != nil {
		t.Fatalf("GetEventsFrom: %v", err)
	}
	if len(got) != 1 || got[0].EventType != models.EventTypeUserMsg {
		t.Fatalf("expected exactly turn2's user_msg, got %d events: %#v", len(got), got)
	}
	var payload models.UserMsgPayload
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if payload.Content != "turn2 user" {
		t.Fatalf("expected the boundary to land on turn2, got %q", payload.Content)
	}
}

// TestGetEventsFromExcludesEventsBeforeTheGivenTurnIndex confirms the window is bounded
// — it must not return events before fromTurnIndex.
func TestGetEventsFromExcludesEventsBeforeTheGivenTurnIndex(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn1 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn1 answer"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn2 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn2 answer"})

	turnIndex, ok, err := repo.FindNthUserMsgTurnIndex(userID, dialogID, 0, 1)
	if err != nil {
		t.Fatalf("FindNthUserMsgTurnIndex: %v", err)
	}
	if !ok {
		t.Fatal("expected a second turn to exist")
	}

	got, err := repo.GetEventsFrom(userID, dialogID, turnIndex)
	if err != nil {
		t.Fatalf("GetEventsFrom: %v", err)
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

// TestGetEventsFromReturnsWholeDialogWhenFromIsZero covers the default state for a
// dialog with no compaction row yet (GetCompactionState returns windowStart 0).
func TestGetEventsFromReturnsWholeDialogWhenFromIsZero(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "only user msg"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "only answer"})

	got, err := repo.GetEventsFrom(userID, dialogID, 0)
	if err != nil {
		t.Fatalf("GetEventsFrom: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both events when from=0, got %d", len(got))
	}
}

func TestFindNthUserMsgTurnIndexNotOKWhenFewerTurnsExist(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "only user msg"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "only answer"})

	_, ok, err := repo.FindNthUserMsgTurnIndex(userID, dialogID, 0, 5)
	if err != nil {
		t.Fatalf("FindNthUserMsgTurnIndex: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when fewer turns exist than requested")
	}
}

func TestCountUserTurnsCountsOnlyUserMsgEventsFromGivenIndex(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn1 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn1 answer"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn2 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn2 answer"})

	count, err := repo.CountUserTurns(userID, dialogID, 0)
	if err != nil {
		t.Fatalf("CountUserTurns: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 turns from the start, got %d", count)
	}

	turnIndex, ok, err := repo.FindNthUserMsgTurnIndex(userID, dialogID, 0, 1)
	if err != nil || !ok {
		t.Fatalf("FindNthUserMsgTurnIndex: ok=%v err=%v", ok, err)
	}
	count, err = repo.CountUserTurns(userID, dialogID, turnIndex)
	if err != nil {
		t.Fatalf("CountUserTurns: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 turn from turn2 onward, got %d", count)
	}
}

func TestGetEventsInRangeExcludesUpperBound(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn1 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn1 answer"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn2 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn2 answer"})

	turnIndex, ok, err := repo.FindNthUserMsgTurnIndex(userID, dialogID, 0, 1)
	if err != nil || !ok {
		t.Fatalf("FindNthUserMsgTurnIndex: ok=%v err=%v", ok, err)
	}

	got, err := repo.GetEventsInRange(userID, dialogID, 0, turnIndex)
	if err != nil {
		t.Fatalf("GetEventsInRange: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly turn1's 2 events (turn2 excluded), got %d: %#v", len(got), got)
	}
}

func TestCompactionStateDefaultsThenPersistsAfterAdvance(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	repo := newTestTraceRepo(t, userID)

	windowStart, summary, err := repo.GetCompactionState(userID, dialogID)
	if err != nil {
		t.Fatalf("GetCompactionState: %v", err)
	}
	if windowStart != 0 || summary != "" {
		t.Fatalf("expected default (0, \"\") for a dialog with no compaction yet, got (%d, %q)", windowStart, summary)
	}

	if err := repo.AdvanceCompaction(userID, dialogID, 3, "earlier turns summarized"); err != nil {
		t.Fatalf("AdvanceCompaction: %v", err)
	}

	windowStart, summary, err = repo.GetCompactionState(userID, dialogID)
	if err != nil {
		t.Fatalf("GetCompactionState: %v", err)
	}
	if windowStart != 3 || summary != "earlier turns summarized" {
		t.Fatalf("expected (3, %q), got (%d, %q)", "earlier turns summarized", windowStart, summary)
	}

	// A second advance must overwrite, not duplicate, the (user_id, dialog_id) row.
	if err := repo.AdvanceCompaction(userID, dialogID, 9, "later turns summarized too"); err != nil {
		t.Fatalf("AdvanceCompaction (second): %v", err)
	}
	windowStart, summary, err = repo.GetCompactionState(userID, dialogID)
	if err != nil {
		t.Fatalf("GetCompactionState: %v", err)
	}
	if windowStart != 9 || summary != "later turns summarized too" {
		t.Fatalf("expected (9, %q), got (%d, %q)", "later turns summarized too", windowStart, summary)
	}
}
