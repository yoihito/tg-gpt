package repositories

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"vadimgribanov.com/tg-gpt/internal/database"
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

// TestGetEventsFromExcludesEventsBeforeTheGivenTurnIndex confirms the window is bounded
// — it must not return events before fromTurnIndex. turn_index is per-event and
// monotonic, so turn2's user_msg (the 3rd event appended) is at turn_index 2.
func TestGetEventsFromExcludesEventsBeforeTheGivenTurnIndex(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	const turn2UserMsgTurnIndex = int64(2)
	repo := newTestTraceRepo(t, userID)

	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn1 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn1 answer"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeUserMsg, models.UserMsgPayload{Content: "turn2 user"})
	mustAppendTraceEvent(t, repo, userID, dialogID, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "turn2 answer"})

	got, err := repo.GetEventsFrom(userID, dialogID, turn2UserMsgTurnIndex)
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

func TestCountUserTurnsCountsOnlyUserMsgEventsFromGivenIndex(t *testing.T) {
	const userID, dialogID = int64(1), int64(1)
	const turn2UserMsgTurnIndex = int64(2)
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

	count, err = repo.CountUserTurns(userID, dialogID, turn2UserMsgTurnIndex)
	if err != nil {
		t.Fatalf("CountUserTurns: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 turn from turn2 onward, got %d", count)
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
