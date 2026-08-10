package services

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/telegram_utils"
)

// fakeTurnRunner stands in for *TextService so TurnDispatcher's per-dialog scheduling,
// coalescing, and cancellation logic can be driven directly, without a real LLM client,
// memory subsystem, or tool set.
type fakeTurnRunner struct {
	mu    sync.Mutex
	calls []fakeTurnCall

	// runFn, if set, replaces the default no-op ("succeed immediately") behavior —
	// tests use it to control exactly when a turn finishes relative to other Submits.
	runFn func(ctx context.Context, user models.User, mctx TurnContext, inputs []UserInput, streamer *telegram_utils.TelegramStreamer) (string, error)
}

type fakeTurnCall struct {
	dialogKey string
	messages  []string
	streamer  *telegram_utils.TelegramStreamer
}

func (f *fakeTurnRunner) PrepareUserForInput(ctx context.Context, user models.User) (models.User, error) {
	return user, nil
}

func (f *fakeTurnRunner) RunAttachedTurn(
	ctx context.Context,
	user models.User,
	mctx TurnContext,
	inputs []UserInput,
	streamer *telegram_utils.TelegramStreamer,
	drainNewInputs func(context.Context) ([]UserInput, error),
) (string, error) {
	messages := make([]string, len(inputs))
	for i, in := range inputs {
		messages[i] = in.Message.Content
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeTurnCall{
		dialogKey: fmt.Sprintf("%d:%d", mctx.UserID, mctx.DialogID),
		messages:  messages,
		streamer:  streamer,
	})
	f.mu.Unlock()

	if f.runFn != nil {
		return f.runFn(ctx, user, mctx, inputs, streamer)
	}
	return "", nil
}

func (f *fakeTurnRunner) allMessagesForDialog(dialogKey string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.dialogKey == dialogKey {
			out = append(out, c.messages...)
		}
	}
	return out
}

func (f *fakeTurnRunner) callCountForDialog(dialogKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.dialogKey == dialogKey {
			n++
		}
	}
	return n
}

func (f *fakeTurnRunner) streamersForDialog(dialogKey string) []*telegram_utils.TelegramStreamer {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*telegram_utils.TelegramStreamer
	for _, c := range f.calls {
		if c.dialogKey == dialogKey {
			out = append(out, c.streamer)
		}
	}
	return out
}

type turnDispatcherHarness struct {
	dispatcher  *TurnDispatcher
	pendingRepo *repositories.PendingInputRepo
	userRepo    *repositories.UserRepo
	fake        *fakeTurnRunner
	user        models.User
}

func newTurnDispatcherHarness(t *testing.T) *turnDispatcherHarness {
	t.Helper()
	db, err := database.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Logf("close db: %v", err)
		}
	})
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	pendingRepo := repositories.NewPendingInputRepo(db)
	traceStore := NewTraceStore(repositories.NewTraceRepo(db))
	userRepo := repositories.NewUserRepo(db)
	fake := &fakeTurnRunner{}
	dispatcher := NewTurnDispatcher(db, pendingRepo, traceStore, fake)

	user, err := userRepo.Register(1001, "Test", "", "test", 5001, true, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	return &turnDispatcherHarness{
		dispatcher:  dispatcher,
		pendingRepo: pendingRepo,
		userRepo:    userRepo,
		fake:        fake,
		user:        user,
	}
}

// testDialogID stands in for a Telegram message thread ID in tests that don't care which
// specific thread is used, only that dialog identity is now caller-supplied rather than
// read off the user.
const testDialogID int64 = 777

func dialogKey(user models.User, dialogID int64) string {
	return fmt.Sprintf("%d:%d", user.Id, dialogID)
}

// waitUntil polls cond until it's true or the timeout elapses, failing the test on
// timeout. Used instead of a fixed sleep since exactly how long the dispatcher takes
// to settle is what these tests are trying not to assume.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met within timeout")
	}
}

func TestTurnDispatcherCoalescesSubmitWhileATurnIsRunning(t *testing.T) {
	h := newTurnDispatcherHarness(t)
	key := dialogKey(h.user, testDialogID)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{})
	var firstCall int32
	h.fake.runFn = func(ctx context.Context, user models.User, mctx TurnContext, inputs []UserInput, streamer *telegram_utils.TelegramStreamer) (string, error) {
		if atomic.AddInt32(&firstCall, 1) == 1 {
			started <- struct{}{}
			<-proceed
		}
		return "", nil
	}

	if err := h.dispatcher.Submit(context.Background(), h.user, testDialogID, 1, llm.Message{Role: llm.RoleUser, Content: "first"}, nil); err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn never started")
	}

	if !h.dispatcher.IsActive(h.user.Id, testDialogID) {
		t.Fatal("expected dialog to be active while its turn is running")
	}

	// Submitted while the first turn is still in flight — must be coalesced into the
	// same dialog's run rather than starting a second, overlapping goroutine.
	if err := h.dispatcher.Submit(context.Background(), h.user, testDialogID, 2, llm.Message{Role: llm.RoleUser, Content: "second"}, nil); err != nil {
		t.Fatal(err)
	}

	close(proceed)

	waitUntil(t, 2*time.Second, func() bool { return !h.dispatcher.IsActive(h.user.Id, testDialogID) })

	got := h.fake.allMessagesForDialog(key)
	want := []string{"first", "second"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("expected both messages processed in submission order, got %#v", got)
	}
}

func TestTurnDispatcherAssignsEachCallTheStreamerOfItsOwnInputs(t *testing.T) {
	h := newTurnDispatcherHarness(t)
	key := dialogKey(h.user, testDialogID)

	started := make(chan struct{}, 1)
	proceed := make(chan struct{})
	var firstCall int32
	h.fake.runFn = func(ctx context.Context, user models.User, mctx TurnContext, inputs []UserInput, streamer *telegram_utils.TelegramStreamer) (string, error) {
		if atomic.AddInt32(&firstCall, 1) == 1 {
			started <- struct{}{}
			<-proceed
		}
		return "", nil
	}

	streamerA := &telegram_utils.TelegramStreamer{}
	streamerB := &telegram_utils.TelegramStreamer{}

	if err := h.dispatcher.Submit(context.Background(), h.user, testDialogID, 1, llm.Message{Role: llm.RoleUser, Content: "first"}, streamerA); err != nil {
		t.Fatal(err)
	}
	<-started

	if err := h.dispatcher.Submit(context.Background(), h.user, testDialogID, 2, llm.Message{Role: llm.RoleUser, Content: "second"}, streamerB); err != nil {
		t.Fatal(err)
	}
	close(proceed)

	waitUntil(t, 2*time.Second, func() bool { return !h.dispatcher.IsActive(h.user.Id, testDialogID) })

	streamers := h.fake.streamersForDialog(key)
	if len(streamers) != 2 || streamers[0] != streamerA || streamers[1] != streamerB {
		t.Fatalf("expected [streamerA, streamerB], got %#v", streamers)
	}
}

func TestTurnDispatcherRunsDifferentDialogsConcurrently(t *testing.T) {
	h := newTurnDispatcherHarness(t)
	user2, err := h.userRepo.Register(1002, "Test2", "", "test2", 5002, true, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan string, 2)
	release := make(chan struct{})
	h.fake.runFn = func(ctx context.Context, user models.User, mctx TurnContext, inputs []UserInput, streamer *telegram_utils.TelegramStreamer) (string, error) {
		started <- fmt.Sprintf("%d:%d", mctx.UserID, mctx.DialogID)
		<-release
		return "", nil
	}

	if err := h.dispatcher.Submit(context.Background(), h.user, testDialogID, 1, llm.Message{Role: llm.RoleUser, Content: "a"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.dispatcher.Submit(context.Background(), user2, testDialogID, 1, llm.Message{Role: llm.RoleUser, Content: "b"}, nil); err != nil {
		t.Fatal(err)
	}

	// If turns for different dialogs were serialized, the second would never start
	// until the first (blocked on <-release) finished, and this would time out.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case key := <-started:
			seen[key] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("expected both dialogs' turns to start concurrently, only saw %v", seen)
		}
	}
	close(release)

	waitUntil(t, 2*time.Second, func() bool {
		return !h.dispatcher.IsActive(h.user.Id, testDialogID) && !h.dispatcher.IsActive(user2.Id, testDialogID)
	})
}

func TestTurnDispatcherCancelDialogStopsRunningTurnAndDiscardsPending(t *testing.T) {
	h := newTurnDispatcherHarness(t)

	started := make(chan struct{}, 1)
	var startedOnce sync.Once
	h.fake.runFn = func(ctx context.Context, user models.User, mctx TurnContext, inputs []UserInput, streamer *telegram_utils.TelegramStreamer) (string, error) {
		startedOnce.Do(func() { started <- struct{}{} })
		<-ctx.Done()
		return "", ctx.Err()
	}

	if err := h.dispatcher.Submit(context.Background(), h.user, testDialogID, 1, llm.Message{Role: llm.RoleUser, Content: "first"}, nil); err != nil {
		t.Fatal(err)
	}
	<-started

	// Coalesced in while the turn above is still blocked — cancellation must discard
	// this too, not just stop the in-flight turn.
	if err := h.dispatcher.Submit(context.Background(), h.user, testDialogID, 2, llm.Message{Role: llm.RoleUser, Content: "second"}, nil); err != nil {
		t.Fatal(err)
	}

	if err := h.dispatcher.CancelDialog(context.Background(), h.user.Id, testDialogID); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 2*time.Second, func() bool { return !h.dispatcher.IsActive(h.user.Id, testDialogID) })

	pending, err := h.pendingRepo.ListPendingForDialog(context.Background(), h.user.Id, testDialogID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected the coalesced-but-never-run input to be discarded on cancel, got %#v", pending)
	}
}
