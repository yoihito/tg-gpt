package services

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

// reminderTestRepos wires just the sqlite-backed repos the reminder modules need,
// without the rest of TextService's dependency graph — the tool-call and scheduler
// halves are tested through their own public interfaces, not through TextService.
type reminderTestRepos struct {
	db           *database.DB
	reminderRepo *repositories.ReminderRepo
	userRepo     *repositories.UserRepo
	prefRepo     *repositories.PreferenceRepo
	traceRepo    *repositories.TraceRepo
}

func newReminderTestRepos(t *testing.T) reminderTestRepos {
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
	repos := reminderTestRepos{
		db:           db,
		reminderRepo: repositories.NewReminderRepo(db),
		userRepo:     repositories.NewUserRepo(db),
		prefRepo:     repositories.NewPreferenceRepo(db),
		traceRepo:    repositories.NewTraceRepo(db),
	}
	if _, err := repos.userRepo.Register(reminderTestUserID, "Test", "", "test", reminderTestChatID, true, "test-model"); err != nil {
		t.Fatal(err)
	}
	return repos
}

const (
	reminderTestUserID = int64(9001)
	reminderTestChatID = int64(4001)
)

func TestReminderToolsCreateOneShotReminderPersists(t *testing.T) {
	repos := newReminderTestRepos(t)
	tools := NewReminderTools(repos.reminderRepo, repos.prefRepo)

	before := time.Now()
	resp, err := tools.HandleToolCall(reminderTestUserID, llm.ToolCall{
		Name:      "create_one_shot_reminder",
		Arguments: `{"time_expression":"in 10 minutes","message":"take out the trash","timezone":"UTC"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp, "Reminder set for") {
		t.Fatalf("unexpected response: %q", resp)
	}

	reminders, err := repos.reminderRepo.GetActiveRemindersForUser(reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reminders) != 1 {
		t.Fatalf("expected 1 active reminder, got %d", len(reminders))
	}
	r := reminders[0]
	if r.Message != "take out the trash" {
		t.Fatalf("message: got %q", r.Message)
	}
	if r.IsRecurring {
		t.Fatal("one-shot reminder should not be recurring")
	}
	wantEarliest := before.Add(9 * time.Minute)
	wantLatest := before.Add(11 * time.Minute)
	if r.RemindAt.Before(wantEarliest) || r.RemindAt.After(wantLatest) {
		t.Fatalf("remind_at %v outside expected window [%v, %v]", r.RemindAt, wantEarliest, wantLatest)
	}
}

func TestReminderToolsCreateOneShotReminderRejectsInvalidTimezone(t *testing.T) {
	repos := newReminderTestRepos(t)
	tools := NewReminderTools(repos.reminderRepo, repos.prefRepo)

	resp, err := tools.HandleToolCall(reminderTestUserID, llm.ToolCall{
		Name:      "create_one_shot_reminder",
		Arguments: `{"time_expression":"in 10 minutes","message":"x","timezone":"Not/AZone"}`,
	})
	if err != nil {
		t.Fatalf("invalid timezone should be reported to the model, not returned as an error: %v", err)
	}
	if !strings.Contains(resp, "Cannot create reminder") {
		t.Fatalf("unexpected response: %q", resp)
	}
	reminders, err := repos.reminderRepo.GetActiveRemindersForUser(reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reminders) != 0 {
		t.Fatalf("expected no reminder to be created, got %d", len(reminders))
	}
}

func TestReminderToolsCreateRecurringReminderPersists(t *testing.T) {
	repos := newReminderTestRepos(t)
	tools := NewReminderTools(repos.reminderRepo, repos.prefRepo)

	resp, err := tools.HandleToolCall(reminderTestUserID, llm.ToolCall{
		Name: "create_recurring_reminder",
		Arguments: `{"message":"stand up meeting","timezone":"UTC","frequency":"weekly",` +
			`"interval":2,"start_at":"2026-06-25T09:00:00"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp, "Recurring reminder set") {
		t.Fatalf("unexpected response: %q", resp)
	}

	reminders, err := repos.reminderRepo.GetActiveRemindersForUser(reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reminders) != 1 {
		t.Fatalf("expected 1 active reminder, got %d", len(reminders))
	}
	r := reminders[0]
	if !r.IsRecurring || r.RecurrenceType == nil || *r.RecurrenceType != models.RecurrenceTypeWeekly {
		t.Fatalf("unexpected recurrence: %#v", r)
	}
	if r.RecurrenceInterval != 2 {
		t.Fatalf("recurrence interval: got %d", r.RecurrenceInterval)
	}
	wantStart := time.Date(2026, 6, 25, 9, 0, 0, 0, time.UTC)
	if !r.RemindAt.Equal(wantStart) {
		t.Fatalf("remind_at: got %v want %v", r.RemindAt, wantStart)
	}
}

func TestReminderToolsListRemindersFormatsActiveReminders(t *testing.T) {
	repos := newReminderTestRepos(t)
	tools := NewReminderTools(repos.reminderRepo, repos.prefRepo)

	if _, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "water the plants",
		RemindAt: time.Now().Add(time.Hour), RecurrenceInterval: 1,
	}); err != nil {
		t.Fatal(err)
	}
	weekly := models.RecurrenceTypeWeekly
	if _, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "team sync",
		RemindAt: time.Now().Add(2 * time.Hour), IsRecurring: true,
		RecurrenceType: &weekly, RecurrenceInterval: 1,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := tools.HandleToolCall(reminderTestUserID, llm.ToolCall{Name: "list_reminders"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp, "water the plants") || !strings.Contains(resp, "team sync") {
		t.Fatalf("expected both reminders listed: %q", resp)
	}
	if !strings.Contains(resp, "repeats weekly every 1") {
		t.Fatalf("expected recurrence noted for the recurring reminder: %q", resp)
	}
}

func TestReminderToolsCancelReminderRemovesFromActiveList(t *testing.T) {
	repos := newReminderTestRepos(t)
	tools := NewReminderTools(repos.reminderRepo, repos.prefRepo)

	id, err := repos.reminderRepo.CreateReminder(models.Reminder{
		UserID: reminderTestUserID, Message: "call the dentist",
		RemindAt: time.Now().Add(time.Hour), RecurrenceInterval: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := tools.HandleToolCall(reminderTestUserID, llm.ToolCall{
		Name:      "cancel_reminder",
		Arguments: fmt.Sprintf(`{"reminder_id":"%d"}`, id),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp, "cancelled") {
		t.Fatalf("unexpected response: %q", resp)
	}

	reminders, err := repos.reminderRepo.GetActiveRemindersForUser(reminderTestUserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reminders) != 0 {
		t.Fatalf("expected no active reminders after cancel, got %d", len(reminders))
	}
}

func TestReminderToolsCancelReminderRejectsUnknownID(t *testing.T) {
	repos := newReminderTestRepos(t)
	tools := NewReminderTools(repos.reminderRepo, repos.prefRepo)

	_, err := tools.HandleToolCall(reminderTestUserID, llm.ToolCall{
		Name:      "cancel_reminder",
		Arguments: `{"reminder_id":"999999"}`,
	})
	if err == nil {
		t.Fatal("expected an error for a nonexistent reminder id")
	}
}

func TestNormalizeReminderActionDefaultsToNotify(t *testing.T) {
	actionType, actionPrompt, err := normalizeReminderAction("", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if actionType != models.ReminderActionNotify {
		t.Fatalf("action type: got %q", actionType)
	}
	if actionPrompt != "" {
		t.Fatalf("notify action should discard action prompt, got %q", actionPrompt)
	}
}

func TestNormalizeReminderActionRequiresPrompt(t *testing.T) {
	_, _, err := normalizeReminderAction("prompt", " ")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "action_prompt is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeReminderActionAcceptsPrompt(t *testing.T) {
	actionType, actionPrompt, err := normalizeReminderAction("prompt", " search the web ")
	if err != nil {
		t.Fatal(err)
	}
	if actionType != models.ReminderActionPrompt {
		t.Fatalf("action type: got %q", actionType)
	}
	if actionPrompt != "search the web" {
		t.Fatalf("action prompt: got %q", actionPrompt)
	}
}
