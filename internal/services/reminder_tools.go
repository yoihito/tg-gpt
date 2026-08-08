package services

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/utils"
)

// isoLocalLayout: ISO 8601 without timezone offset; interpreted in the supplied
// *time.Location at parse time.
const isoLocalLayout = "2006-01-02T15:04:05"

// ReminderTools exposes reminder management (create/list/cancel) as LLM tool calls. It
// owns no scheduling or delivery — see ReminderScheduler for firing reminders once due.
type ReminderTools struct {
	reminderRepo *repositories.ReminderRepo
	prefRepo     *repositories.PreferenceRepo
	timeParser   *utils.TimeParser
}

func NewReminderTools(
	reminderRepo *repositories.ReminderRepo,
	prefRepo *repositories.PreferenceRepo,
) *ReminderTools {
	return &ReminderTools{
		reminderRepo: reminderRepo,
		prefRepo:     prefRepo,
		timeParser:   utils.NewTimeParser(),
	}
}

func (s *ReminderTools) GetReminderTools() []llm.Tool {
	return []llm.Tool{
		{
			Name:        "create_one_shot_reminder",
			Description: "Create a single, non-repeating reminder. Use this whenever the user wants to be reminded exactly once.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"time_expression": map[string]interface{}{
						"type":        "string",
						"description": "Natural language time: 'in 5 minutes', 'tomorrow at 3pm', 'next Monday at 9am'.",
					},
					"message": map[string]interface{}{
						"type":        "string",
						"description": "The reminder message in the user's language.",
					},
					"timezone": map[string]interface{}{
						"type":        "string",
						"description": "User's IANA timezone (e.g. 'Europe/Berlin'). Read it from the user's preferences shown in the system prompt; if it's not there, ask the user and save it as preference 'timezone' before calling this tool.",
					},
					"action_type": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"notify", "prompt"},
						"description": "Use notify for simple reminders. Use prompt when the user wants the bot to perform a scheduled action later.",
					},
					"action_prompt": map[string]interface{}{
						"type":        "string",
						"description": "Instruction to execute at reminder time. Required when action_type is prompt. Scheduled actions can only use web_search.",
					},
				},
				"required": []string{"time_expression", "message", "timezone"},
			},
		},
		{
			Name:        "create_recurring_reminder",
			Description: "Create a reminder that repeats on a schedule. Use whenever the user mentions repetition (daily, weekly, monthly, every N days/weeks/months).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message": map[string]interface{}{
						"type":        "string",
						"description": "The reminder message in the user's language.",
					},
					"timezone": map[string]interface{}{
						"type":        "string",
						"description": "User's IANA timezone. Read from preferences; ask the user if missing.",
					},
					"frequency": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"daily", "weekly", "monthly"},
						"description": "Base unit of recurrence.",
					},
					"interval": map[string]interface{}{
						"type":        "integer",
						"description": "Repeat every N units of frequency. interval=2 + frequency=weekly = biweekly. Defaults to 1 if omitted.",
					},
					"start_at": map[string]interface{}{
						"type":        "string",
						"description": "ISO 8601 datetime (no timezone suffix) of the first occurrence, in the user's local timezone. Example: '2026-06-25T09:00:00'.",
					},
					"until": map[string]interface{}{
						"type":        "string",
						"description": "Optional ISO 8601 datetime after which recurrence stops. Omit for indefinite recurrence.",
					},
					"action_type": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"notify", "prompt"},
						"description": "Use notify for simple reminders. Use prompt when the user wants the bot to perform a scheduled action each time.",
					},
					"action_prompt": map[string]interface{}{
						"type":        "string",
						"description": "Instruction to execute at reminder time. Required when action_type is prompt. Scheduled actions can only use web_search.",
					},
				},
				"required": []string{"message", "timezone", "frequency", "start_at"},
			},
		},
		{
			Name:        "list_reminders",
			Description: "List all active (not-yet-fired, not-cancelled) reminders for the user.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "cancel_reminder",
			Description: "Cancel a reminder by its ID. For recurring reminders, this cancels all future occurrences.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"reminder_id": map[string]interface{}{
						"type":        "string",
						"description": "The ID of the reminder to cancel.",
					},
				},
				"required": []string{"reminder_id"},
			},
		},
	}
}

func (s *ReminderTools) HandleToolCall(userID int64, toolCall llm.ToolCall) (string, error) {
	if toolCall.Name == "list_reminders" {
		return s.handleListReminders(userID)
	}

	if err := ValidateToolArguments(toolCall.Name, toolCall.Arguments); err != nil {
		return "", err
	}

	switch toolCall.Name {
	case "create_one_shot_reminder":
		return s.handleCreateOneShotReminder(userID, toolCall.Arguments)
	case "create_recurring_reminder":
		return s.handleCreateRecurringReminder(userID, toolCall.Arguments)
	case "cancel_reminder":
		return s.handleCancelReminder(userID, toolCall.Arguments)
	default:
		return "", fmt.Errorf("unknown tool call: %s", toolCall.Name)
	}
}

func (s *ReminderTools) handleCreateOneShotReminder(userID int64, arguments string) (string, error) {
	var args struct {
		TimeExpression string `json:"time_expression"`
		Message        string `json:"message"`
		Timezone       string `json:"timezone"`
		ActionType     string `json:"action_type"`
		ActionPrompt   string `json:"action_prompt"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for create_one_shot_reminder: %w", err)
	}
	actionType, actionPrompt, err := normalizeReminderAction(args.ActionType, args.ActionPrompt)
	if err != nil {
		return err.Error(), nil
	}

	loc, err := utils.GetUserTimezone(args.Timezone)
	if err != nil {
		return fmt.Sprintf("Cannot create reminder: %s. Ask the user for their IANA timezone (e.g. 'Europe/Berlin').", err), nil
	}

	parsedTime, err := s.timeParser.ParseTimeOnly(args.TimeExpression, time.Now().In(loc), loc)
	if err != nil {
		return fmt.Sprintf("Could not understand time expression %q. Ask the user to rephrase.", args.TimeExpression), nil
	}

	id, err := s.reminderRepo.CreateReminder(models.Reminder{
		UserID:             userID,
		Message:            args.Message,
		RemindAt:           *parsedTime,
		IsRecurring:        false,
		RecurrenceInterval: 1,
		ActionType:         actionType,
		ActionPrompt:       actionPrompt,
	})
	if err != nil {
		slog.Error("Failed to create one-shot reminder", "error", err, "user_id", userID)
		return "Failed to create reminder", err
	}
	timeStr := parsedTime.In(loc).Format("Mon Jan 2, 2006 at 3:04 PM MST")
	slog.Info("One-shot reminder created", "reminder_id", id, "user_id", userID, "remind_at", parsedTime, "timezone", args.Timezone)
	if actionType == models.ReminderActionPrompt {
		return fmt.Sprintf("Scheduled action set for %s: %s", timeStr, args.Message), nil
	}
	return fmt.Sprintf("Reminder set for %s: %s", timeStr, args.Message), nil
}

func (s *ReminderTools) handleCreateRecurringReminder(userID int64, arguments string) (string, error) {
	var args struct {
		Message      string `json:"message"`
		Timezone     string `json:"timezone"`
		Frequency    string `json:"frequency"`
		Interval     int    `json:"interval"`
		StartAt      string `json:"start_at"`
		Until        string `json:"until"`
		ActionType   string `json:"action_type"`
		ActionPrompt string `json:"action_prompt"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for create_recurring_reminder: %w", err)
	}
	actionType, actionPrompt, err := normalizeReminderAction(args.ActionType, args.ActionPrompt)
	if err != nil {
		return err.Error(), nil
	}

	loc, err := utils.GetUserTimezone(args.Timezone)
	if err != nil {
		return fmt.Sprintf("Cannot create reminder: %s. Ask the user for their IANA timezone (e.g. 'Europe/Berlin').", err), nil
	}

	var rType models.RecurrenceType
	switch args.Frequency {
	case "daily":
		rType = models.RecurrenceTypeDaily
	case "weekly":
		rType = models.RecurrenceTypeWeekly
	case "monthly":
		rType = models.RecurrenceTypeMonthly
	default:
		return fmt.Sprintf("Invalid frequency %q. Must be 'daily', 'weekly', or 'monthly'.", args.Frequency), nil
	}

	startAt, err := time.ParseInLocation(isoLocalLayout, args.StartAt, loc)
	if err != nil {
		return fmt.Sprintf("Invalid start_at %q. Use ISO 8601 like '2026-06-25T09:00:00'.", args.StartAt), nil
	}

	var until *time.Time
	if strings.TrimSpace(args.Until) != "" {
		u, err := time.ParseInLocation(isoLocalLayout, args.Until, loc)
		if err != nil {
			return fmt.Sprintf("Invalid until %q. Use ISO 8601 like '2026-12-31T23:59:59'.", args.Until), nil
		}
		until = &u
	}

	interval := args.Interval
	if interval < 1 {
		interval = 1
	}

	id, err := s.reminderRepo.CreateReminder(models.Reminder{
		UserID:             userID,
		Message:            args.Message,
		RemindAt:           startAt,
		IsRecurring:        true,
		RecurrenceType:     &rType,
		RecurrenceInterval: interval,
		RecurrenceEndAt:    until,
		ActionType:         actionType,
		ActionPrompt:       actionPrompt,
	})
	if err != nil {
		slog.Error("Failed to create recurring reminder", "error", err, "user_id", userID)
		return "Failed to create reminder", err
	}

	timeStr := startAt.In(loc).Format("Mon Jan 2, 2006 at 3:04 PM MST")
	label := "Recurring reminder set"
	if actionType == models.ReminderActionPrompt {
		label = "Recurring scheduled action set"
	}
	resp := fmt.Sprintf("%s, first fire %s (repeats %s every %d). Message: %s", label, timeStr, args.Frequency, interval, args.Message)
	if until != nil {
		resp += fmt.Sprintf(" until %s", until.In(loc).Format("Mon Jan 2, 2006"))
	}
	slog.Info("Recurring reminder created",
		"reminder_id", id,
		"user_id", userID,
		"frequency", args.Frequency,
		"interval", interval,
		"start_at", startAt,
		"timezone", args.Timezone,
	)
	return resp, nil
}

func normalizeReminderAction(actionTypeRaw, actionPromptRaw string) (models.ReminderActionType, string, error) {
	actionType := models.ReminderActionType(strings.TrimSpace(actionTypeRaw))
	if actionType == "" {
		actionType = models.ReminderActionNotify
	}
	actionPrompt := strings.TrimSpace(actionPromptRaw)
	switch actionType {
	case models.ReminderActionNotify:
		return actionType, "", nil
	case models.ReminderActionPrompt:
		if actionPrompt == "" {
			return "", "", fmt.Errorf("Cannot create scheduled action: action_prompt is required when action_type is prompt.")
		}
		return actionType, actionPrompt, nil
	default:
		return "", "", fmt.Errorf("Invalid action_type %q. Must be 'notify' or 'prompt'.", actionTypeRaw)
	}
}

func (s *ReminderTools) handleListReminders(userID int64) (string, error) {
	reminders, err := s.reminderRepo.GetActiveRemindersForUser(userID)
	if err != nil {
		return "", err
	}
	if len(reminders) == 0 {
		return "No active reminders found", nil
	}

	loc, _ := lookupUserTimezone(s.prefRepo, userID)
	if loc == nil {
		loc = time.UTC
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You have %d active reminder(s):\n", len(reminders))
	for i, r := range reminders {
		fmt.Fprintf(&b, "%d. [ID: %d] %s - %s",
			i+1, r.ID, r.RemindAt.In(loc).Format("Mon Jan 2 at 3:04 PM"), r.Message)
		if r.IsRecurring && r.RecurrenceType != nil {
			fmt.Fprintf(&b, " (repeats %s every %d)", *r.RecurrenceType, r.RecurrenceInterval)
		}
		if r.ActionType == models.ReminderActionPrompt {
			fmt.Fprintf(&b, " [scheduled action]")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (s *ReminderTools) handleCancelReminder(userID int64, arguments string) (string, error) {
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
	if _, err := s.reminderRepo.GetReminderByID(reminderID, userID); err != nil {
		return "", fmt.Errorf("reminder not found: %w", err)
	}
	if err := s.reminderRepo.CancelReminder(reminderID, userID); err != nil {
		return "", fmt.Errorf("failed to cancel reminder: %w", err)
	}
	slog.Info("Reminder cancelled", "reminder_id", reminderID, "user_id", userID)
	return fmt.Sprintf("Reminder #%d cancelled", reminderID), nil
}

// lookupUserTimezone resolves a user's stored 'timezone' preference. Both ReminderTools
// (listing reminders in the user's local time) and ReminderScheduler (calculating a
// recurring reminder's next occurrence) need this same lookup at different points in
// the reminder lifecycle.
func lookupUserTimezone(prefRepo *repositories.PreferenceRepo, userID int64) (*time.Location, error) {
	pref, err := prefRepo.Get(userID, "timezone")
	if err != nil {
		return nil, fmt.Errorf("read timezone preference: %w", err)
	}
	if pref == nil {
		return nil, fmt.Errorf("user has no 'timezone' preference")
	}
	loc, err := time.LoadLocation(pref.PrefValue)
	if err != nil {
		return nil, fmt.Errorf("invalid stored timezone %q: %w", pref.PrefValue, err)
	}
	return loc, nil
}
