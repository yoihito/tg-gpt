package repositories

import (
	"database/sql"
	"fmt"
	"time"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type ReminderRepo struct {
	db *database.DB
}

func NewReminderRepo(db *database.DB) *ReminderRepo {
	return &ReminderRepo{db: db}
}

// CreateReminder inserts a new reminder. Timezone is NOT stored on the row — it
// lives in preference_memory and is fetched at fire time for recurring reminders.
func (r *ReminderRepo) CreateReminder(reminder models.Reminder) (int64, error) {
	actionType := reminder.ActionType
	if actionType == "" {
		actionType = models.ReminderActionNotify
	}
	var recurrenceType sql.NullString
	if reminder.RecurrenceType != nil {
		recurrenceType = sql.NullString{String: string(*reminder.RecurrenceType), Valid: true}
	}
	var recurrenceEndAt sql.NullInt64
	if reminder.RecurrenceEndAt != nil {
		recurrenceEndAt = sql.NullInt64{Int64: reminder.RecurrenceEndAt.Unix(), Valid: true}
	}

	result, err := r.db.Exec(`
		INSERT INTO reminders (
			user_id, message, remind_at,
			is_recurring, recurrence_type, recurrence_interval, recurrence_end_at,
			action_type, action_prompt
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		reminder.UserID,
		reminder.Message,
		reminder.RemindAt.Unix(),
		reminder.IsRecurring,
		recurrenceType,
		reminder.RecurrenceInterval,
		recurrenceEndAt,
		string(actionType),
		reminder.ActionPrompt,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to create reminder: %w", err)
	}
	return result.LastInsertId()
}

// GetActiveRemindersForUser returns reminders that haven't fired or been cancelled.
// For recurring reminders, only the next-pending row exists (we update in place),
// so this is the user-visible list.
func (r *ReminderRepo) GetActiveRemindersForUser(userID int64) ([]models.Reminder, error) {
	rows, err := r.db.Query(`
		SELECT id, user_id, message, remind_at, created_at, updated_at,
		       is_fired, is_cancelled, is_processing,
		       is_recurring, recurrence_type, recurrence_interval,
		       recurrence_end_at, last_fired_at, processing_started_at,
		       action_type, action_prompt
		FROM reminders
		WHERE user_id = ? AND is_cancelled = false AND is_fired = false
		ORDER BY remind_at ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query reminders: %w", err)
	}
	defer rows.Close()
	return r.scanReminders(rows)
}

func (r *ReminderRepo) GetDueReminders(before time.Time) ([]models.Reminder, error) {
	staleProcessingBefore := before.Add(-30 * time.Minute)
	rows, err := r.db.Query(`
		SELECT id, user_id, message, remind_at, created_at, updated_at,
		       is_fired, is_cancelled, is_processing,
		       is_recurring, recurrence_type, recurrence_interval,
		       recurrence_end_at, last_fired_at, processing_started_at,
		       action_type, action_prompt
		FROM reminders
		WHERE remind_at <= ?
		  AND is_fired = false
		  AND is_cancelled = false
		  AND (is_processing = false OR processing_started_at < ?)
		ORDER BY remind_at ASC
		LIMIT 100
	`, before.Unix(), staleProcessingBefore.Unix())
	if err != nil {
		return nil, fmt.Errorf("failed to query due reminders: %w", err)
	}
	defer rows.Close()
	return r.scanReminders(rows)
}

func (r *ReminderRepo) ClaimReminder(reminderID int64, now time.Time) (bool, error) {
	staleProcessingBefore := now.Add(-30 * time.Minute)
	result, err := r.db.Exec(`
		UPDATE reminders
		SET is_processing = true,
		    processing_started_at = ?,
		    updated_at = strftime('%s', 'now')
		WHERE id = ?
		  AND is_fired = false
		  AND is_cancelled = false
		  AND (is_processing = false OR processing_started_at < ?)
	`, now.Unix(), reminderID, staleProcessingBefore.Unix())
	if err != nil {
		return false, fmt.Errorf("failed to claim reminder: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check claimed reminder rows: %w", err)
	}
	return rows > 0, nil
}

func (r *ReminderRepo) ReleaseReminderClaim(reminderID int64) error {
	_, err := r.db.Exec(`
		UPDATE reminders
		SET is_processing = false,
		    processing_started_at = NULL,
		    updated_at = strftime('%s', 'now')
		WHERE id = ?
	`, reminderID)
	if err != nil {
		return fmt.Errorf("failed to release reminder claim: %w", err)
	}
	return nil
}

// MarkReminderFired marks a one-shot reminder as fired.
func (r *ReminderRepo) MarkReminderFired(reminderID int64, firedAt time.Time) error {
	_, err := r.db.Exec(`
		UPDATE reminders
		SET is_fired = true,
		    is_processing = false,
		    last_fired_at = ?,
		    processing_started_at = NULL,
		    updated_at = strftime('%s', 'now')
		WHERE id = ?
	`, firedAt.Unix(), reminderID)
	if err != nil {
		return fmt.Errorf("failed to mark reminder as fired: %w", err)
	}
	return nil
}

// UpdateNextOccurrence reschedules a recurring reminder in place. is_fired flips
// back to false so the scheduler will fire it again at the new remind_at.
func (r *ReminderRepo) UpdateNextOccurrence(reminderID int64, newRemindAt, lastFiredAt time.Time) error {
	_, err := r.db.Exec(`
		UPDATE reminders
		SET remind_at = ?,
		    is_fired = false,
		    is_processing = false,
		    last_fired_at = ?,
		    processing_started_at = NULL,
		    updated_at = strftime('%s', 'now')
		WHERE id = ?
	`, newRemindAt.Unix(), lastFiredAt.Unix(), reminderID)
	if err != nil {
		return fmt.Errorf("failed to update next occurrence: %w", err)
	}
	return nil
}

func (r *ReminderRepo) CancelReminder(reminderID int64, userID int64) error {
	result, err := r.db.Exec(`
		UPDATE reminders
		SET is_cancelled = true,
		    is_processing = false,
		    processing_started_at = NULL,
		    updated_at = strftime('%s', 'now')
		WHERE id = ? AND user_id = ?
	`, reminderID, userID)
	if err != nil {
		return fmt.Errorf("failed to cancel reminder: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("reminder not found or unauthorized")
	}
	return nil
}

func (r *ReminderRepo) GetReminderByID(reminderID int64, userID int64) (*models.Reminder, error) {
	row := r.db.QueryRow(`
		SELECT id, user_id, message, remind_at, created_at, updated_at,
		       is_fired, is_cancelled, is_processing,
		       is_recurring, recurrence_type, recurrence_interval,
		       recurrence_end_at, last_fired_at, processing_started_at,
		       action_type, action_prompt
		FROM reminders
		WHERE id = ? AND user_id = ?
	`, reminderID, userID)
	reminder, err := r.scanReminder(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("reminder not found")
		}
		return nil, fmt.Errorf("failed to get reminder: %w", err)
	}
	return &reminder, nil
}

func (r *ReminderRepo) scanReminders(rows *sql.Rows) ([]models.Reminder, error) {
	var reminders []models.Reminder
	for rows.Next() {
		var reminder models.Reminder
		var recurrenceType sql.NullString
		var recurrenceEndAt, lastFiredAt, processingStartedAt sql.NullInt64
		var actionType, actionPrompt sql.NullString
		var remindAt, createdAt, updatedAt int64

		err := rows.Scan(
			&reminder.ID,
			&reminder.UserID,
			&reminder.Message,
			&remindAt,
			&createdAt,
			&updatedAt,
			&reminder.IsFired,
			&reminder.IsCancelled,
			&reminder.IsProcessing,
			&reminder.IsRecurring,
			&recurrenceType,
			&reminder.RecurrenceInterval,
			&recurrenceEndAt,
			&lastFiredAt,
			&processingStartedAt,
			&actionType,
			&actionPrompt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan reminder: %w", err)
		}

		reminder.RemindAt = time.Unix(remindAt, 0)
		reminder.CreatedAt = time.Unix(createdAt, 0)
		reminder.UpdatedAt = time.Unix(updatedAt, 0)

		if recurrenceType.Valid {
			rt := models.RecurrenceType(recurrenceType.String)
			reminder.RecurrenceType = &rt
		}
		if recurrenceEndAt.Valid {
			t := time.Unix(recurrenceEndAt.Int64, 0)
			reminder.RecurrenceEndAt = &t
		}
		if lastFiredAt.Valid {
			t := time.Unix(lastFiredAt.Int64, 0)
			reminder.LastFiredAt = &t
		}
		if processingStartedAt.Valid {
			t := time.Unix(processingStartedAt.Int64, 0)
			reminder.ProcessingStartedAt = &t
		}
		reminder.ActionType = models.ReminderActionNotify
		if actionType.Valid && actionType.String != "" {
			reminder.ActionType = models.ReminderActionType(actionType.String)
		}
		if actionPrompt.Valid {
			reminder.ActionPrompt = actionPrompt.String
		}

		reminders = append(reminders, reminder)
	}
	return reminders, nil
}

func (r *ReminderRepo) scanReminder(row *sql.Row) (models.Reminder, error) {
	var reminder models.Reminder
	var recurrenceType sql.NullString
	var recurrenceEndAt, lastFiredAt, processingStartedAt sql.NullInt64
	var actionType, actionPrompt sql.NullString
	var remindAt, createdAt, updatedAt int64

	err := row.Scan(
		&reminder.ID,
		&reminder.UserID,
		&reminder.Message,
		&remindAt,
		&createdAt,
		&updatedAt,
		&reminder.IsFired,
		&reminder.IsCancelled,
		&reminder.IsProcessing,
		&reminder.IsRecurring,
		&recurrenceType,
		&reminder.RecurrenceInterval,
		&recurrenceEndAt,
		&lastFiredAt,
		&processingStartedAt,
		&actionType,
		&actionPrompt,
	)
	if err != nil {
		return models.Reminder{}, err
	}

	reminder.RemindAt = time.Unix(remindAt, 0)
	reminder.CreatedAt = time.Unix(createdAt, 0)
	reminder.UpdatedAt = time.Unix(updatedAt, 0)

	if recurrenceType.Valid {
		rt := models.RecurrenceType(recurrenceType.String)
		reminder.RecurrenceType = &rt
	}
	if recurrenceEndAt.Valid {
		t := time.Unix(recurrenceEndAt.Int64, 0)
		reminder.RecurrenceEndAt = &t
	}
	if lastFiredAt.Valid {
		t := time.Unix(lastFiredAt.Int64, 0)
		reminder.LastFiredAt = &t
	}
	if processingStartedAt.Valid {
		t := time.Unix(processingStartedAt.Int64, 0)
		reminder.ProcessingStartedAt = &t
	}
	reminder.ActionType = models.ReminderActionNotify
	if actionType.Valid && actionType.String != "" {
		reminder.ActionType = models.ReminderActionType(actionType.String)
	}
	if actionPrompt.Valid {
		reminder.ActionPrompt = actionPrompt.String
	}
	return reminder, nil
}
