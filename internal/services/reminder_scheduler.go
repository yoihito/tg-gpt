package services

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/telegram_utils"
)

type ScheduledActionRunner interface {
	RunScheduledAction(ctx context.Context, user models.User, reminder models.Reminder) (string, error)
}

// ReminderScheduler owns the reminder lifecycle once a reminder exists: polling for due
// reminders, claiming and firing them, and rescheduling recurring ones. It owns no
// tool-call parsing or validation — see ReminderTools for how reminders get created.
type ReminderScheduler struct {
	reminderRepo *repositories.ReminderRepo
	userRepo     *repositories.UserRepo
	prefRepo     *repositories.PreferenceRepo
	trace        *TraceStore
	bot          *tele.Bot
	actionRunner ScheduledActionRunner

	ticker    *time.Ticker
	stopChan  chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	isRunning bool
}

func NewReminderScheduler(
	reminderRepo *repositories.ReminderRepo,
	userRepo *repositories.UserRepo,
	prefRepo *repositories.PreferenceRepo,
	trace *TraceStore,
	bot *tele.Bot,
) *ReminderScheduler {
	return &ReminderScheduler{
		reminderRepo: reminderRepo,
		userRepo:     userRepo,
		prefRepo:     prefRepo,
		trace:        trace,
		bot:          bot,
		stopChan:     make(chan struct{}),
	}
}

func (s *ReminderScheduler) SetScheduledActionRunner(actionRunner ScheduledActionRunner) {
	s.actionRunner = actionRunner
}

func (s *ReminderScheduler) StartScheduler(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isRunning {
		return fmt.Errorf("scheduler already running")
	}
	s.ticker = time.NewTicker(30 * time.Second)
	s.isRunning = true
	s.wg.Add(1)
	go s.schedulerLoop(ctx)
	slog.InfoContext(ctx, "Reminder scheduler started")
	return nil
}

func (s *ReminderScheduler) StopScheduler(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isRunning {
		return nil
	}
	slog.InfoContext(ctx, "Stopping reminder scheduler")
	close(s.stopChan)
	s.ticker.Stop()
	s.wg.Wait()
	s.isRunning = false
	slog.InfoContext(ctx, "Reminder scheduler stopped")
	return nil
}

func (s *ReminderScheduler) schedulerLoop(ctx context.Context) {
	defer s.wg.Done()
	slog.InfoContext(ctx, "Scheduler loop started")
	for {
		select {
		case <-s.stopChan:
			slog.InfoContext(ctx, "Scheduler loop stopping")
			return
		case <-s.ticker.C:
			s.checkAndFireReminders(ctx)
		}
	}
}

func (s *ReminderScheduler) checkAndFireReminders(ctx context.Context) {
	dueReminders, err := s.reminderRepo.GetDueReminders(time.Now())
	if err != nil {
		slog.ErrorContext(ctx, "Failed to fetch due reminders", "error", err)
		return
	}
	if len(dueReminders) == 0 {
		return
	}
	slog.InfoContext(ctx, "Found due reminders", "count", len(dueReminders))
	for _, reminder := range dueReminders {
		s.fireReminder(ctx, reminder)
	}
}

func (s *ReminderScheduler) fireReminder(ctx context.Context, reminder models.Reminder) {
	slog.InfoContext(ctx, "Firing reminder", "reminder_id", reminder.ID, "user_id", reminder.UserID)

	claimed, err := s.reminderRepo.ClaimReminder(reminder.ID, time.Now())
	if err != nil {
		slog.ErrorContext(ctx, "Failed to claim reminder", "error", err, "reminder_id", reminder.ID)
		return
	}
	if !claimed {
		slog.InfoContext(ctx, "Reminder already claimed or closed", "reminder_id", reminder.ID)
		return
	}

	user, err := s.userRepo.GetUser(reminder.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get user for reminder", "error", err, "user_id", reminder.UserID)
		s.releaseReminderClaim(ctx, reminder.ID)
		return
	}

	if reminder.ActionType == models.ReminderActionPrompt {
		s.fireScheduledAction(ctx, user, reminder)
		return
	}

	naturalMessages := []string{
		"Hey! Just wanted to remind you: %s",
		"Hi there! You asked me to remind you: %s",
		"Reminder! Don't forget: %s",
		"Hey, it's time! Remember: %s",
		"Quick reminder: %s",
		"Just a heads up: %s",
	}
	messageFormat := naturalMessages[time.Now().UnixNano()%int64(len(naturalMessages))]
	naturalMessage := fmt.Sprintf(messageFormat, reminder.Message)

	sentMsg, err := s.bot.Send(&tele.User{ID: user.ChatId}, naturalMessage)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to send reminder", "error", err, "reminder_id", reminder.ID)
		s.releaseReminderClaim(ctx, reminder.ID)
		return
	}

	if err := s.userRepo.Touch(user.Id, time.Now().Unix()); err != nil {
		slog.ErrorContext(ctx, "Failed to update user last interaction", "error", err, "user_id", reminder.UserID)
	}

	syntheticUserText := fmt.Sprintf("[Reminder triggered for: %s]", reminder.Message)
	if err := s.trace.RecordReminderFire(user.Id, user.CurrentDialogId, syntheticUserText, naturalMessage, int64(sentMsg.ID)); err != nil {
		slog.ErrorContext(ctx, "Failed to save reminder to trace", "error", err, "reminder_id", reminder.ID)
	}

	if reminder.IsRecurring && !reminder.HasExpiredRecurrence() {
		s.rescheduleRecurring(ctx, reminder)
		return
	}

	if err := s.reminderRepo.MarkReminderFired(reminder.ID, time.Now()); err != nil {
		slog.ErrorContext(ctx, "Failed to mark reminder as fired", "error", err, "reminder_id", reminder.ID)
	}
	slog.InfoContext(ctx, "Reminder fired", "reminder_id", reminder.ID)
}

func (s *ReminderScheduler) fireScheduledAction(ctx context.Context, user models.User, reminder models.Reminder) {
	if s.actionRunner == nil {
		slog.ErrorContext(ctx, "No scheduled action runner configured", "reminder_id", reminder.ID)
		s.releaseReminderClaim(ctx, reminder.ID)
		return
	}

	response, err := s.actionRunner.RunScheduledAction(ctx, user, reminder)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to run scheduled action", "error", err, "reminder_id", reminder.ID)
		s.releaseReminderClaim(ctx, reminder.ID)
		return
	}

	if strings.TrimSpace(response) == "" {
		response = "Scheduled action completed, but returned no text."
	}
	if err := telegram_utils.SendChunked(s.bot, &tele.User{ID: user.ChatId}, response); err != nil {
		slog.ErrorContext(ctx, "Failed to send scheduled action result", "error", err, "reminder_id", reminder.ID)
		s.releaseReminderClaim(ctx, reminder.ID)
		return
	}

	if reminder.IsRecurring && !reminder.HasExpiredRecurrence() {
		s.rescheduleRecurring(ctx, reminder)
		return
	}

	if err := s.reminderRepo.MarkReminderFired(reminder.ID, time.Now()); err != nil {
		slog.ErrorContext(ctx, "Failed to mark scheduled action as fired", "error", err, "reminder_id", reminder.ID)
	}
	slog.InfoContext(ctx, "Scheduled action fired", "reminder_id", reminder.ID)
}

func (s *ReminderScheduler) releaseReminderClaim(ctx context.Context, reminderID int64) {
	if err := s.reminderRepo.ReleaseReminderClaim(reminderID); err != nil {
		slog.ErrorContext(ctx, "Failed to release reminder claim", "error", err, "reminder_id", reminderID)
	}
}

func (s *ReminderScheduler) rescheduleRecurring(ctx context.Context, reminder models.Reminder) {
	loc, err := lookupUserTimezone(s.prefRepo, reminder.UserID)
	if err != nil {
		slog.WarnContext(ctx, "No timezone preference for recurring reminder; falling back to UTC", "error", err, "reminder_id", reminder.ID)
		loc = time.UTC
	}

	nextTime := reminder.CalculateNextOccurrence(loc)
	if nextTime == nil {
		// Recurrence ended (past until-date or unsupported type) — close it out.
		if err := s.reminderRepo.MarkReminderFired(reminder.ID, time.Now()); err != nil {
			slog.ErrorContext(ctx, "Failed to mark expired recurring reminder", "error", err, "reminder_id", reminder.ID)
		}
		slog.InfoContext(ctx, "Recurring reminder closed (no more occurrences)", "reminder_id", reminder.ID)
		return
	}

	if err := s.reminderRepo.UpdateNextOccurrence(reminder.ID, *nextTime, time.Now()); err != nil {
		slog.ErrorContext(ctx, "Failed to update next occurrence", "error", err, "reminder_id", reminder.ID)
		return
	}
	slog.InfoContext(ctx, "Recurring reminder rescheduled",
		"reminder_id", reminder.ID,
		"next_time", nextTime,
	)
}
