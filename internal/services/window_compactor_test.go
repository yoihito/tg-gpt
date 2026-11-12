package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

const (
	compactorTestUserID   = int64(5001)
	compactorTestDialogID = int64(99)
)

func newCompactorTestTraceRepo(t *testing.T) *repositories.TraceRepo {
	t.Helper()
	db, err := database.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.NewUserRepo(db).Register(compactorTestUserID, "Test", "", "test", compactorTestUserID, true, "test-model"); err != nil {
		t.Fatal(err)
	}
	return repositories.NewTraceRepo(db)
}

// newFakeSummarizer returns a Summarizer backed by a server that always replies with
// the given canned summary, plus a channel of the raw request bodies it received (so
// tests can assert on what got sent to the model).
func newFakeSummarizer(t *testing.T, canned string) (*Summarizer, chan string) {
	t.Helper()
	bodies := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp","object":"response","status":"completed","output":[{"id":"msg","type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}`, canned)
	}))
	t.Cleanup(server.Close)
	client := openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/v1"))
	return NewSummarizer(&client, "test-model"), bodies
}

func appendUserModelTurn(t *testing.T, repo *repositories.TraceRepo, userID, dialogID int64, userText, modelText string) {
	t.Helper()
	if _, err := repo.Append(repositories.AppendEventInput{
		UserID: userID, DialogID: dialogID,
		EventType: models.EventTypeUserMsg, Payload: models.UserMsgPayload{Content: userText},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Append(repositories.AppendEventInput{
		UserID: userID, DialogID: dialogID,
		EventType: models.EventTypeModelMsg, Payload: models.ModelMsgPayload{Content: modelText},
	}); err != nil {
		t.Fatal(err)
	}
}

// turnText is a fixed-length filler string so every appended turn contributes the same,
// easy-to-reason-about estimateTokens() value in these tests.
const turnText = "0123456789012345678901234567890123456789" // 40 chars -> estimateTokens == 10

func TestMaybeCompactNoopWhenCurrentInputTokensBelowHighWaterMark(t *testing.T) {
	repo := newCompactorTestTraceRepo(t)
	summarizer, _ := newFakeSummarizer(t, "should not be used")
	compactor := NewWindowCompactor(repo, summarizer, 50, 100)

	for i := 0; i < 20; i++ {
		appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, turnText, turnText)
	}

	// currentInputTokens itself is below the high-water mark, regardless of how much
	// content is sitting in the live window.
	compactor.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID, 100)

	windowStart, summary, err := repo.GetCompactionState(compactorTestUserID, compactorTestDialogID)
	if err != nil {
		t.Fatal(err)
	}
	if windowStart != 0 || summary != "" {
		t.Fatalf("expected no compaction yet, got (%d, %q)", windowStart, summary)
	}
}

func TestMaybeCompactNoopWhenWindowContentAlreadyBelowLowWaterMark(t *testing.T) {
	repo := newCompactorTestTraceRepo(t)
	summarizer, _ := newFakeSummarizer(t, "should not be used")
	// highWater is tiny so the trigger always fires; lowWater is huge so the window's
	// own (small) content never looks worth evicting.
	compactor := NewWindowCompactor(repo, summarizer, 10000, 1)

	appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, turnText, turnText)
	appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, turnText, turnText)

	compactor.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID, 999999)

	windowStart, summary, err := repo.GetCompactionState(compactorTestUserID, compactorTestDialogID)
	if err != nil {
		t.Fatal(err)
	}
	if windowStart != 0 || summary != "" {
		t.Fatalf("expected no compaction when the window itself is small, got (%d, %q)", windowStart, summary)
	}
}

func TestMaybeCompactAlwaysKeepsAtLeastTheMostRecentTurn(t *testing.T) {
	repo := newCompactorTestTraceRepo(t)
	summarizer, bodies := newFakeSummarizer(t, "should not be used")
	compactor := NewWindowCompactor(repo, summarizer, 1, 1)

	appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, turnText, turnText)

	compactor.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID, 999999)

	select {
	case <-bodies:
		t.Fatal("expected the summarizer not to be called with only one turn in the window")
	default:
	}
	windowStart, summary, err := repo.GetCompactionState(compactorTestUserID, compactorTestDialogID)
	if err != nil {
		t.Fatal(err)
	}
	if windowStart != 0 || summary != "" {
		t.Fatalf("expected no compaction with a single turn, got (%d, %q)", windowStart, summary)
	}
}

func TestMaybeCompactEvictsOldestTurnsUntilUnderLowWaterMark(t *testing.T) {
	repo := newCompactorTestTraceRepo(t)
	summarizer, bodies := newFakeSummarizer(t, "Folded summary of the oldest turns.")
	tokensPerTurn := estimateTokens(turnText) * 2 // user + model message
	const totalTurns = 8
	const lowWaterTokens = int64(50)
	compactor := NewWindowCompactor(repo, summarizer, int(lowWaterTokens), 1)

	for i := 0; i < totalTurns; i++ {
		appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, turnText, turnText)
	}

	// Walk the same eviction rule MaybeCompact uses to compute how many of the
	// totalTurns turns it should evict, so the test doesn't hardcode a number that
	// would silently go stale if turnText or lowWaterTokens above changes.
	remaining := tokensPerTurn * totalTurns
	evicted := 0
	for evicted < totalTurns-1 && remaining > lowWaterTokens {
		remaining -= tokensPerTurn
		evicted++
	}
	wantRemainingTurns := int64(totalTurns - evicted)

	compactor.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID, 999999)

	select {
	case <-bodies:
	default:
		t.Fatal("expected the summarizer to be called")
	}

	windowStart, summary, err := repo.GetCompactionState(compactorTestUserID, compactorTestDialogID)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Folded summary of the oldest turns." {
		t.Fatalf("unexpected rolling summary: %q", summary)
	}

	remainingTurns, err := repo.CountUserTurns(compactorTestUserID, compactorTestDialogID, windowStart)
	if err != nil {
		t.Fatal(err)
	}
	if remainingTurns != wantRemainingTurns {
		t.Fatalf("expected %d turns left in the live window, got %d", wantRemainingTurns, remainingTurns)
	}

	// The evicted range (everything before windowStart) must now be excluded from a
	// fresh replay — exactly what Retriever.Retrieve will do via GetEventsFrom.
	replay, err := repo.GetEventsFrom(compactorTestUserID, compactorTestDialogID, windowStart)
	if err != nil {
		t.Fatal(err)
	}
	userTurnsInReplay := 0
	for _, e := range replay {
		if e.EventType == models.EventTypeUserMsg {
			userTurnsInReplay++
		}
	}
	if int64(userTurnsInReplay) != wantRemainingTurns {
		t.Fatalf("expected replay to contain exactly the %d remaining turns, got %d", wantRemainingTurns, userTurnsInReplay)
	}
}

func TestMaybeCompactCarriesPreviousSummaryForwardOnNextEviction(t *testing.T) {
	repo := newCompactorTestTraceRepo(t)
	summarizer, bodies := newFakeSummarizer(t, "first summary")
	compactor := NewWindowCompactor(repo, summarizer, 50, 1)

	for i := 0; i < 8; i++ {
		appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, turnText, turnText)
	}
	compactor.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID, 999999)
	select {
	case <-bodies:
	default:
		t.Fatal("expected the first eviction to call the summarizer")
	}

	summarizer2, bodies2 := newFakeSummarizer(t, "second summary")
	compactor2 := NewWindowCompactor(repo, summarizer2, 50, 1)

	for i := 0; i < 8; i++ { // enough new turns to cross the low-water mark again
		appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, turnText, turnText)
	}
	compactor2.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID, 999999)

	var body string
	select {
	case body = <-bodies2:
	default:
		t.Fatal("expected the second eviction to call the summarizer")
	}
	if !strings.Contains(body, "first summary") {
		t.Fatalf("expected the previous rolling summary to be sent as context, got body: %s", body)
	}

	_, summary, err := repo.GetCompactionState(compactorTestUserID, compactorTestDialogID)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "second summary" {
		t.Fatalf("expected the rolling summary to be replaced with the new one, got %q", summary)
	}
}
