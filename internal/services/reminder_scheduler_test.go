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
	mu       sync.Mutex
	messages []string
	server   *httptest.Server
}

func newFakeTelegramServer(t *testing.T) *fakeTelegramServer {
	t.Helper()
	f := &fakeTelegramServer{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.messages = append(f.messages, body.Text)
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

	if sent := fakeTg.sentMessages(); len(sent) != 0 {
		t.Fatalf("expected no message sent for an already-claimed reminder, got %#v", sent)
	}
}
