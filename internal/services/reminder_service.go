package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/utils"
)

type ReminderService struct {
	reminderRepo *repositories.ReminderRepo
	userRepo     *repositories.UserRepo
	messagesRepo *repositories.MessagesRepo
	timeParser   *utils.TimeParser
	bot          *tele.Bot

	// Scheduler management
	ticker    *time.Ticker
	stopChan  chan struct{}
	wg        sync.WaitGroup
	mu        sync.Mutex
	isRunning bool
}

func NewReminderService(
	reminderRepo *repositories.ReminderRepo,
	userRepo *repositories.UserRepo,
	messagesRepo *repositories.MessagesRepo,
	bot *tele.Bot,
) *ReminderService {
	return &ReminderService{
		reminderRepo: reminderRepo,
		userRepo:     userRepo,
		messagesRepo: messagesRepo,
		timeParser:   utils.NewTimeParser(),
		bot:          bot,
		stopChan:     make(chan struct{}),
	}
}

// GetReminderTools returns tool definitions for LLM
func (s *ReminderService) GetReminderTools() []openai.Tool {
	return []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "create_reminder",
				Description: "Create a reminder for the user. IMPORTANT: Always check your memory for the user's timezone. If you don't have it, ask the user for their timezone/location and save it to memory before creating the reminder.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"time_expression": map[string]interface{}{
							"type":        "string",
							"description": "Time expression in English (e.g., 'in 5 minutes', 'tomorrow at 3pm', 'next Monday at 9am', 'daily at 8am', 'every week')",
						},
						"message": map[string]interface{}{
							"type":        "string",
							"description": "The reminder message in the user's language",
						},
						"timezone": map[string]interface{}{
							"type":        "string",
							"description": "User's timezone in IANA format (e.g., 'America/New_York', 'Europe/Moscow', 'Asia/Tokyo'). Retrieve from your memory or ask the user.",
						},
					},
					"required": []string{"time_expression", "message", "timezone"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "list_reminders",
				Description: "List all active reminders for the user.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "cancel_reminder",
				Description: "Cancel a specific reminder by its ID.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"reminder_id": map[string]interface{}{
							"type":        "string",
							"description": "The ID of the reminder to cancel",
						},
					},
					"required": []string{"reminder_id"},
				},
			},
		},
	}
}

// HandleToolCall routes tool calls to appropriate handlers
func (s *ReminderService) HandleToolCall(userID int64, toolCall openai.ToolCall) (string, error) {
	// Special handling for list_reminders which doesn't need arguments
	if toolCall.Function.Name == "list_reminders" {
		return s.handleListReminders(userID)
	}

	// Validate that we have complete arguments for other tool calls
	if strings.TrimSpace(toolCall.Function.Arguments) == "" {
		return "", fmt.Errorf("empty arguments for tool call: %s", toolCall.Function.Name)
	}

	// Try to validate JSON structure
	var test map[string]interface{}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &test); err != nil {
		return "", fmt.Errorf("invalid JSON arguments for %s: %w - arguments: %s", toolCall.Function.Name, err, toolCall.Function.Arguments)
	}

	switch toolCall.Function.Name {
	case "create_reminder":
		return s.handleCreateReminder(userID, toolCall.Function.Arguments)
	case "cancel_reminder":
		return s.handleCancelReminder(userID, toolCall.Function.Arguments)
	default:
		return "", fmt.Errorf("unknown tool call: %s", toolCall.Function.Name)
	}
}

// handleCreateReminder creates a new reminder
func (s *ReminderService) handleCreateReminder(userID int64, arguments string) (string, error) {
	var args struct {
		TimeExpression string `json:"time_expression"`
		Message        string `json:"message"`
		Timezone       string `json:"timezone"`
	}

	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for create_reminder: %w", err)
	}

	// Load user's timezone
	loc, err := utils.GetUserTimezone(args.Timezone)
	if err != nil {
		slog.Warn("Invalid timezone provided, falling back to UTC", "timezone", args.Timezone, "error", err)
		loc = time.UTC
		args.Timezone = "UTC"
	}

	// Check for recurrence pattern in time expression
	recurrenceType, interval, timeExpressionCleaned := s.timeParser.ParseRecurrence(args.TimeExpression)

	// Parse the time from the English time expression using user's timezone
	// The reference time should be in the user's timezone
	referenceTime := time.Now().In(loc)
	parsedTime, _, err := s.timeParser.ParseTime(timeExpressionCleaned+" placeholder", referenceTime, loc)
	if err != nil {
		return "", fmt.Errorf("could not understand time expression '%s': %w", args.TimeExpression, err)
	}

	// Validate time is in the future
	if parsedTime.Before(time.Now()) {
		return "", fmt.Errorf("reminder time must be in the future")
	}

	reminder := models.Reminder{
		UserID:             userID,
		Message:            args.Message,
		RemindAt:           *parsedTime,
		Timezone:           args.Timezone,
		IsRecurring:        recurrenceType != nil,
		RecurrenceInterval: interval,
	}

	if recurrenceType != nil {
		rt := models.RecurrenceType(*recurrenceType)
		reminder.RecurrenceType = &rt
	}

	id, err := s.reminderRepo.CreateReminder(reminder)
	if err != nil {
		slog.Error("Failed to create reminder", "error", err, "user_id", userID)
		return "Failed to create reminder", err
	}

	// Format response in user's timezone
	timeStr := parsedTime.In(loc).Format("Mon Jan 2, 2006 at 3:04 PM MST")
	response := fmt.Sprintf("Reminder set for %s: %s", timeStr, args.Message)

	if reminder.IsRecurring && reminder.RecurrenceType != nil {
		response += fmt.Sprintf(" (Repeats: %s)", *reminder.RecurrenceType)
	}

	slog.Info("Reminder created", "reminder_id", id, "user_id", userID, "remind_at", parsedTime, "timezone", args.Timezone)

	return response, nil
}

// handleListReminders lists all active reminders for a user
func (s *ReminderService) handleListReminders(userID int64) (string, error) {
	reminders, err := s.reminderRepo.GetActiveRemindersForUser(userID)
	if err != nil {
		return "", err
	}

	if len(reminders) == 0 {
		return "No active reminders found", nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("You have %d active reminder(s):\n", len(reminders)))

	for i, reminder := range reminders {
		timeStr := reminder.RemindAt.Format("Mon Jan 2 at 3:04 PM")
		status := "pending"
		if reminder.IsFired {
			status = "fired"
		}

		result.WriteString(fmt.Sprintf("%d. [ID: %d] %s - %s (%s)\n",
			i+1, reminder.ID, timeStr, reminder.Message, status))

		if reminder.IsRecurring && reminder.RecurrenceType != nil {
			result.WriteString(fmt.Sprintf("   Repeats: %s\n", *reminder.RecurrenceType))
		}
	}

	return result.String(), nil
}

// handleCancelReminder cancels a specific reminder
func (s *ReminderService) handleCancelReminder(userID int64, arguments string) (string, error) {
	var args struct {
		ReminderID string `json:"reminder_id"`
	}

	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for cancel_reminder: %w", err)
	}

	reminderID, err := strconv.ParseInt(args.ReminderID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid reminder ID: %w", err)
	}

	// Verify reminder exists and belongs to user
	_, err = s.reminderRepo.GetReminderByID(reminderID, userID)
	if err != nil {
		return "", fmt.Errorf("reminder not found: %w", err)
	}

	err = s.reminderRepo.CancelReminder(reminderID, userID)
	if err != nil {
		return "", fmt.Errorf("failed to cancel reminder: %w", err)
	}

	slog.Info("Reminder cancelled", "reminder_id", reminderID, "user_id", userID)
	return fmt.Sprintf("Reminder #%d cancelled", reminderID), nil
}

// StartScheduler begins the background reminder checker
func (s *ReminderService) StartScheduler(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isRunning {
		return fmt.Errorf("scheduler already running")
	}

	s.ticker = time.NewTicker(30 * time.Second) // Check every 30 seconds
	s.isRunning = true

	s.wg.Add(1)
	go s.schedulerLoop(ctx)

	slog.InfoContext(ctx, "Reminder scheduler started")
	return nil
}

// StopScheduler gracefully stops the scheduler
func (s *ReminderService) StopScheduler(ctx context.Context) error {
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

// schedulerLoop is the main background loop
func (s *ReminderService) schedulerLoop(ctx context.Context) {
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

// checkAndFireReminders polls for due reminders and sends them
func (s *ReminderService) checkAndFireReminders(ctx context.Context) {
	now := time.Now()

	dueReminders, err := s.reminderRepo.GetDueReminders(now)
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

// fireReminder sends a reminder to the user
func (s *ReminderService) fireReminder(ctx context.Context, reminder models.Reminder) {
	slog.InfoContext(ctx, "Firing reminder", "reminder_id", reminder.ID, "user_id", reminder.UserID)

	// Get user to retrieve chat ID
	user, err := s.userRepo.GetUser(reminder.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get user for reminder", "error", err, "user_id", reminder.UserID)
		return
	}

	// Create natural reminder messages
	naturalMessages := []string{
		"Hey! Just wanted to remind you: %s",
		"Hi there! You asked me to remind you: %s",
		"Reminder! Don't forget: %s",
		"Hey, it's time! Remember: %s",
		"Quick reminder: %s",
		"Just a heads up: %s",
	}

	// Select a random natural message format
	messageFormat := naturalMessages[time.Now().UnixNano()%int64(len(naturalMessages))]
	naturalMessage := fmt.Sprintf(messageFormat, reminder.Message)

	// Send reminder message to Telegram
	sentMsg, err := s.bot.Send(&tele.User{ID: user.ChatId}, naturalMessage)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to send reminder", "error", err, "reminder_id", reminder.ID)
		return
	}

	// Touch user to update last_interaction time
	user.Touch()
	if err := s.userRepo.UpdateUser(user); err != nil {
		slog.ErrorContext(ctx, "Failed to update user last interaction", "error", err, "user_id", reminder.UserID)
	}

	// Save reminder to conversation history as an interaction
	// Create a synthetic user message to pair with the assistant's reminder
	userMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: fmt.Sprintf("[Reminder triggered for: %s]", reminder.Message),
	}

	assistantMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: naturalMessage,
	}

	interaction := models.Interaction{
		UserMessage:      userMessage,
		AssistantMessage: assistantMessage,
		AuthorId:         user.Id,
		DialogId:         user.CurrentDialogId,
		TgUserMessageId:  0, // No actual user message
		TgAssistantMessageId: int64(sentMsg.ID),
	}

	if err := s.messagesRepo.AddMessage(interaction); err != nil {
		slog.ErrorContext(ctx, "Failed to save reminder to conversation history", "error", err, "reminder_id", reminder.ID)
	}

	// Mark as fired
	err = s.reminderRepo.MarkReminderFired(reminder.ID, time.Now())
	if err != nil {
		slog.ErrorContext(ctx, "Failed to mark reminder as fired", "error", err, "reminder_id", reminder.ID)
	}

	// Handle recurring reminders
	if reminder.IsRecurring && !reminder.HasExpiredRecurrence() {
		nextTime := reminder.CalculateNextOccurrence()
		if nextTime != nil {
			err = s.reminderRepo.ScheduleNextOccurrence(reminder, *nextTime)
			if err != nil {
				slog.ErrorContext(ctx, "Failed to schedule next occurrence", "error", err, "reminder_id", reminder.ID)
			} else {
				slog.InfoContext(ctx, "Scheduled next occurrence", "reminder_id", reminder.ID, "next_time", nextTime)
			}
		}
	}

	slog.InfoContext(ctx, "Reminder fired successfully", "reminder_id", reminder.ID)
}
