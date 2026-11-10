package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

// fakeTelegramServer records every sendMessage call a *tele.Bot makes against it and
// answers with the minimum valid Telegram API response the client library needs.
type fakeTelegramServer struct {
	mu        sync.Mutex
	messages  []string
	threadIDs []string
	server    *httptest.Server
}

func newFakeTelegramServer(t *testing.T) *fakeTelegramServer {
	t.Helper()
	f := &fakeTelegramServer{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body struct {
				Text     string `json:"text"`
				ThreadID string `json:"message_thread_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.messages = append(f.messages, body.Text)
			f.threadIDs = append(f.threadIDs, body.ThreadID)
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTelegramServer) sentMessages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.messages))
	copy(out, f.messages)
	return out
}

func (f *fakeTelegramServer) sentThreadIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.threadIDs))
	copy(out, f.threadIDs)
	return out
}

func newTestBot(t *testing.T, url string) *tele.Bot {
	t.Helper()
	bot, err := tele.NewBot(tele.Settings{Token: "test-token", URL: url, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func TestReminderSchedulerFiresDueOneShotReminder(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	traceStore := NewTraceStore(repos.traceRepo)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, traceStore, newTestBot(t, fakeTg.server.URL))

	id, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "buy milk",
		RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()

	sent := fakeTg.sentMessages()
	if len(sent) != 1 || !strings.Contains(sent[0], "buy milk") {
		t.Fatalf("expected one message mentioning the reminder, got %#v", sent)
	}

	fired, err := repos.reminderRepo.GetReminderByID(id, reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if !fired.IsFired {
		t.Fatal("expected reminder to be marked fired")
	}
	if fired.IsProcessing {
		t.Fatal("expected claim to be released once fired")
	}
}

func TestReminderSchedulerRoutesNotifyReminderToOriginThread(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	traceStore := NewTraceStore(repos.traceRepo)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, traceStore, newTestBot(t, fakeTg.server.URL))

	const originDialogID = int64(555)
	if _, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, DialogID: originDialogID, Message: "water the plants",
		RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1,
	}); err != nil {
		t.Fatal(err)
	}

	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()

	threads := fakeTg.sentThreadIDs()
	if len(threads) != 1 || threads[0] != "555" {
		t.Fatalf("expected the reminder routed to its origin thread (555), got %v", threads)
	}

	events, err := repos.traceRepo.GetAllForDialog(reminderTestUserID, originDialogID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected the reminder fire to be recorded in the origin thread's trace")
	}
}

func TestReminderSchedulerSkipsReminderNotYetDue(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	traceStore := NewTraceStore(repos.traceRepo)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, traceStore, newTestBot(t, fakeTg.server.URL))

	if _, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "future reminder",
		RemindAt: time.Now().Add(time.Hour), RecurrenceInterval: 1,
	}); err != nil {
		t.Fatal(err)
	}

	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()

	if sent := fakeTg.sentMessages(); len(sent) != 0 {
		t.Fatalf("expected no messages sent for a not-yet-due reminder, got %#v", sent)
	}
}

func TestReminderSchedulerReschedulesRecurringReminderAfterFiring(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	traceStore := NewTraceStore(repos.traceRepo)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, traceStore, newTestBot(t, fakeTg.server.URL))

	if err := repos.prefRepo.Upsert(repositories.UpsertPreferenceInput{
		UserID: reminderTestUserID, Key: "timezone", Value: "UTC", Source: "explicit",
	}); err != nil {
		t.Fatal(err)
	}

	daily := models.RecurrenceTypeDaily
	firstRun := time.Now().Add(-time.Minute)
	id, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "daily standup",
		RemindAt: firstRun, IsRecurring: true,
		RecurrenceType: &daily, RecurrenceInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()

	rescheduled, err := repos.reminderRepo.GetReminderByID(id, reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if rescheduled.IsFired {
		t.Fatal("recurring reminder should not be marked fired — it was rescheduled instead")
	}
	// Stored and reloaded at second precision, so compare at that precision too.
	wantNext := firstRun.AddDate(0, 0, 1).Unix()
	if rescheduled.RemindAt.Unix() != wantNext {
		t.Fatalf("remind_at: got %v want unix %d (one day after the previous occurrence)", rescheduled.RemindAt, wantNext)
	}

	if sent := fakeTg.sentMessages(); len(sent) != 1 || !strings.Contains(sent[0], "daily standup") {
		t.Fatalf("expected the recurring reminder to still fire once before rescheduling, got %#v", sent)
	}
}

// TestNextFutureOccurrenceFastForwardsThroughDrift is the regression test for a
// recurring reminder that fell far behind (e.g. an hourly check whose base RemindAt was
// hours in the past): CalculateNextOccurrence only ever advances one interval, so
// without fast-forwarding, rescheduleRecurring would persist a next_time that's still
// due immediately, and the scheduler's 30s poll would re-fire it over and over (running
// the full scheduled action, LLM calls included) until enough ticks had passed to catch
// up one hour at a time — "every hour" effectively becoming "every 30 seconds" for
// however long the drift was.
func TestNextFutureOccurrenceFastForwardsThroughDrift(t *testing.T) {
	hourly := models.RecurrenceTypeHourly
	now := time.Now()
	reminder := models.Reminder{
		RemindAt:           now.Add(-5*time.Hour - 10*time.Minute), // drifted ~5h behind
		IsRecurring:        true,
		RecurrenceType:     &hourly,
		RecurrenceInterval: 1,
	}

	next := nextFutureOccurrence(reminder, time.UTC, now)
	if next == nil {
		t.Fatal("expected a next occurrence")
	}
	if !next.After(now) {
		t.Fatalf("expected the fast-forwarded occurrence to be in the future, got %v (now=%v)", next, now)
	}
	// It should land on the *next* hourly slot after now, not jump arbitrarily far
	// ahead: at most one interval beyond now.
	if next.Sub(now) > time.Hour {
		t.Fatalf("expected the next occurrence within one hour of now, got %v ahead", next.Sub(now))
	}
}

func TestReminderSchedulerFiresOnceForHourlyReminderThatDriftedFarBehind(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	traceStore := NewTraceStore(repos.traceRepo)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, traceStore, newTestBot(t, fakeTg.server.URL))

	if err := repos.prefRepo.Upsert(repositories.UpsertPreferenceInput{
		UserID: reminderTestUserID, Key: "timezone", Value: "UTC", Source: "explicit",
	}); err != nil {
		t.Fatal(err)
	}

	hourly := models.RecurrenceTypeHourly
	id, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "hourly check",
		RemindAt: time.Now().Add(-5 * time.Hour), IsRecurring: true,
		RecurrenceType: &hourly, RecurrenceInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()

	if sent := fakeTg.sentMessages(); len(sent) != 1 {
		t.Fatalf("expected exactly one fire despite 5h of drift, got %d: %#v", len(sent), sent)
	}

	rescheduled, err := repos.reminderRepo.GetReminderByID(id, reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if !rescheduled.RemindAt.After(time.Now()) {
		t.Fatalf("expected the rescheduled time to be in the future, got %v", rescheduled.RemindAt)
	}

	// Firing again immediately must be a no-op: the schedule has already caught up.
	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()
	if sent := fakeTg.sentMessages(); len(sent) != 1 {
		t.Fatalf("expected no additional fire right after catching up, got %d: %#v", len(sent), sent)
	}
}

func TestReminderSchedulerDoesNotDoubleFireAConcurrentlyClaimedReminder(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	traceStore := NewTraceStore(repos.traceRepo)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, traceStore, newTestBot(t, fakeTg.server.URL))

	id, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "shared reminder",
		RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a second scheduler instance (e.g. two ticks racing) already having
	// claimed this reminder — checkAndFireReminders should see it's unclaimable and
	// skip it instead of firing a duplicate notification.
	claimed, err := repos.reminderRepo.ClaimReminder(id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("expected the direct claim to succeed")
	}

	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()

	if sent := fakeTg.sentMessages(); len(sent) != 0 {
		t.Fatalf("expected no message sent for an already-claimed reminder, got %#v", sent)
	}
}

type scheduledRunnerFunc func(context.Context, models.User, models.Reminder) (bool, string, error)

func (f scheduledRunnerFunc) RunScheduledAction(ctx context.Context, user models.User, reminder models.Reminder) (bool, string, error) {
	return f(ctx, user, reminder)
}

func TestFireScheduledActionSkipsSendWhenAgentHasNothingToReport(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, NewTraceStore(repos.traceRepo), newTestBot(t, fakeTg.server.URL))
	scheduler.SetScheduledActionRunner(scheduledRunnerFunc(func(context.Context, models.User, models.Reminder) (bool, string, error) {
		return false, "", nil
	}))
	id, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "check news", ActionType: models.ReminderActionPrompt, ActionPrompt: "check",
		RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()

	if sent := fakeTg.sentMessages(); len(sent) != 0 {
		t.Fatalf("expected no message when the agent found nothing to report, got %#v", sent)
	}
	fired, err := repos.reminderRepo.GetReminderByID(id, reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if !fired.IsFired || fired.IsProcessing {
		t.Fatal("a silent check should still be marked fired, same as a notified one")
	}
}

func TestFireScheduledActionSendsToOriginThreadWhenNotified(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, NewTraceStore(repos.traceRepo), newTestBot(t, fakeTg.server.URL))
	scheduler.SetScheduledActionRunner(scheduledRunnerFunc(func(context.Context, models.User, models.Reminder) (bool, string, error) {
		return true, "breaking news!", nil
	}))
	if _, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, DialogID: 42, Message: "check news",
		ActionType: models.ReminderActionPrompt, ActionPrompt: "check",
		RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1,
	}); err != nil {
		t.Fatal(err)
	}

	scheduler.checkAndFireReminders(context.Background())
	scheduler.wg.Wait()

	sent := fakeTg.sentMessages()
	if len(sent) != 1 || sent[0] != "breaking news!" {
		t.Fatalf("expected the notify_user message to be sent, got %#v", sent)
	}
	threads := fakeTg.sentThreadIDs()
	if len(threads) != 1 || threads[0] != "42" {
		t.Fatalf("expected the message routed to its origin thread (42), got %v", threads)
	}
}

func TestSchedulerStopCancelsFloodWaitAndAllowsRestart(t *testing.T) {
	repos := newReminderTestRepos(t)
	flooded := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		select {
		case flooded <- struct{}{}:
		default:
		}
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 230","parameters":{"retry_after":230}}`))
	}))
	defer server.Close()
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, NewTraceStore(repos.traceRepo), newTestBot(t, server.URL))
	scheduler.SetScheduledActionRunner(scheduledRunnerFunc(func(context.Context, models.User, models.Reminder) (bool, string, error) {
		return true, "scheduled answer", nil
	}))
	id, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "scheduled action", RemindAt: time.Now().Add(-time.Minute),
		ActionType: models.ReminderActionPrompt, ActionPrompt: "answer", RecurrenceInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := scheduler.StartScheduler(parent); err != nil {
		t.Fatal(err)
	}
	scheduler.ticker.Reset(time.Millisecond)
	select {
	case <-flooded:
	case <-time.After(time.Second):
		cancel()
		_ = scheduler.StopScheduler(parent)
		t.Fatal("scheduled delivery never attempted")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- scheduler.StopScheduler(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("StopScheduler did not cancel flood wait")
	}
	if parent.Err() != nil {
		t.Fatal("shutdown required parent context cancellation")
	}
	reminder, err := repos.reminderRepo.GetReminderByID(id, reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if reminder.IsProcessing || reminder.IsFired {
		t.Fatal("failed delivery should release its claim without marking fired")
	}
	if err := scheduler.StartScheduler(parent); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.StopScheduler(parent); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledDeliveryTimeoutReleasesClaim(t *testing.T) {
	repos := newReminderTestRepos(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 230","parameters":{"retry_after":230}}`))
	}))
	defer server.Close()
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, NewTraceStore(repos.traceRepo), newTestBot(t, server.URL))
	scheduler.deliveryTimeout = 20 * time.Millisecond
	scheduler.SetScheduledActionRunner(scheduledRunnerFunc(func(context.Context, models.User, models.Reminder) (bool, string, error) {
		return true, "scheduled answer", nil
	}))
	id, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "action", ActionType: models.ReminderActionPrompt,
		RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.checkAndFireReminders(context.Background())
	done := make(chan struct{})
	go func() { scheduler.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delivery timeout did not bound retries")
	}
	reminder, err := repos.reminderRepo.GetReminderByID(id, reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if reminder.IsProcessing || reminder.IsFired {
		t.Fatal("timed-out delivery should remain eligible for retry")
	}
}

func TestFloodedScheduledActionDoesNotBlockOtherUsers(t *testing.T) {
	repos := newReminderTestRepos(t)
	otherUserID := reminderTestUserID + 1
	if _, err := repos.userRepo.Register(otherUserID, "Other", "", "", reminderTestChatID+1, true, "test-model"); err != nil {
		t.Fatal(err)
	}
	flooded := make(chan struct{}, 1)
	notified := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if body.Text == "scheduled answer" {
			select {
			case flooded <- struct{}{}:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 230","parameters":{"retry_after":230}}`))
		} else {
			select {
			case notified <- struct{}{}:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":1}}}`))
		}
	}))
	defer server.Close()
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, NewTraceStore(repos.traceRepo), newTestBot(t, server.URL))
	scheduler.SetScheduledActionRunner(scheduledRunnerFunc(func(context.Context, models.User, models.Reminder) (bool, string, error) {
		return true, "scheduled answer", nil
	}))
	for _, reminder := range []models.Reminder{
		{UserID: reminderTestUserID, Message: "action", ActionType: models.ReminderActionPrompt, RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1},
		{UserID: otherUserID, Message: "buy milk", RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1},
	} {
		if _, err := repos.reminderRepo.CreateReminder(reminder); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); scheduler.wg.Wait() }()
	scheduler.checkAndFireReminders(ctx)
	select {
	case <-flooded:
	case <-time.After(time.Second):
		t.Fatal("expected flood response")
	}
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("flood wait blocked other user")
	}
}

func TestSchedulerKeepsActionsSequentialPerUser(t *testing.T) {
	repos := newReminderTestRepos(t)
	fakeTg := newFakeTelegramServer(t)
	scheduler := NewReminderScheduler(repos.reminderRepo, repos.userRepo, repos.prefRepo, NewTraceStore(repos.traceRepo), newTestBot(t, fakeTg.server.URL))
	started := make(chan struct{}, 2)
	scheduler.SetScheduledActionRunner(scheduledRunnerFunc(func(ctx context.Context, _ models.User, _ models.Reminder) (bool, string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return false, "", ctx.Err()
	}))
	for i := 0; i < 2; i++ {
		if _, err := repos.reminderRepo.CreateReminder(models.Reminder{UserID: reminderTestUserID, Message: "action", ActionType: models.ReminderActionPrompt, RemindAt: time.Now().Add(-time.Minute), RecurrenceInterval: 1}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); scheduler.wg.Wait() }()
	scheduler.checkAndFireReminders(ctx)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("action never started")
	}
	scheduler.checkAndFireReminders(ctx)
	select {
	case <-started:
		t.Fatal("two actions ran concurrently for the same user")
	case <-time.After(20 * time.Millisecond):
	}
}
