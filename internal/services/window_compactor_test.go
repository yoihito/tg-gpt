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

func TestMaybeCompactNoopBelowHighWaterMark(t *testing.T) {
	repo := newCompactorTestTraceRepo(t)
	summarizer, _ := newFakeSummarizer(t, "should not be used")
	compactor := NewWindowCompactor(repo, summarizer, 6, 6)

	for i := 0; i < 11; i++ { // lowWater+batchSize == 12, so 11 turns must stay untouched
		appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i))
	}

	compactor.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID)

	windowStart, summary, err := repo.GetCompactionState(compactorTestUserID, compactorTestDialogID)
	if err != nil {
		t.Fatal(err)
	}
	if windowStart != 0 || summary != "" {
		t.Fatalf("expected no compaction yet, got (%d, %q)", windowStart, summary)
	}
}

func TestMaybeCompactEvictsOldestBatchOnceOverHighWaterMark(t *testing.T) {
	repo := newCompactorTestTraceRepo(t)
	summarizer, bodies := newFakeSummarizer(t, "Folded summary of the oldest 6 turns.")
	compactor := NewWindowCompactor(repo, summarizer, 6, 6)

	for i := 0; i < 13; i++ { // crosses lowWater(6)+batchSize(6)=12
		appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i))
	}

	compactor.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID)

	select {
	case <-bodies:
	default:
		t.Fatal("expected the summarizer to be called")
	}

	windowStart, summary, err := repo.GetCompactionState(compactorTestUserID, compactorTestDialogID)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Folded summary of the oldest 6 turns." {
		t.Fatalf("unexpected rolling summary: %q", summary)
	}

	remaining, err := repo.CountUserTurns(compactorTestUserID, compactorTestDialogID, windowStart)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 7 { // 13 - 6 evicted
		t.Fatalf("expected 7 turns left in the live window, got %d", remaining)
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
	if userTurnsInReplay != 7 {
		t.Fatalf("expected replay to contain exactly the 7 remaining turns, got %d", userTurnsInReplay)
	}
}

func TestMaybeCompactCarriesPreviousSummaryForwardOnNextEviction(t *testing.T) {
	repo := newCompactorTestTraceRepo(t)
	summarizer, bodies := newFakeSummarizer(t, "first summary")
	compactor := NewWindowCompactor(repo, summarizer, 6, 6)

	for i := 0; i < 13; i++ { // crosses the high-water mark once (lowWater+batchSize=12)
		appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i))
	}
	compactor.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID)
	select {
	case <-bodies:
	default:
		t.Fatal("expected the first eviction to call the summarizer")
	}

	summarizer2, bodies2 := newFakeSummarizer(t, "second summary")
	compactor2 := NewWindowCompactor(repo, summarizer2, 6, 6)

	for i := 13; i < 26; i++ { // another 13 turns pushes it over the mark a second time
		appendUserModelTurn(t, repo, compactorTestUserID, compactorTestDialogID, fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i))
	}
	compactor2.MaybeCompact(context.Background(), compactorTestUserID, compactorTestDialogID)

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
