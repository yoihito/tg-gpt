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

// CreateReminder inserts a new reminder
func (r *ReminderRepo) CreateReminder(reminder models.Reminder) (int64, error) {
	query := `
		INSERT INTO reminders (
			user_id, message, remind_at, timezone,
			is_recurring, recurrence_type, recurrence_interval, recurrence_end_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`

	var recurrenceType sql.NullString
	if reminder.RecurrenceType != nil {
		recurrenceType = sql.NullString{String: string(*reminder.RecurrenceType), Valid: true}
	}

	var recurrenceEndAt sql.NullInt64
	if reminder.RecurrenceEndAt != nil {
		recurrenceEndAt = sql.NullInt64{Int64: reminder.RecurrenceEndAt.Unix(), Valid: true}
	}

	result, err := r.db.Exec(query,
		reminder.UserID,
		reminder.Message,
		reminder.RemindAt.Unix(),
		reminder.Timezone,
		reminder.IsRecurring,
		recurrenceType,
		reminder.RecurrenceInterval,
		recurrenceEndAt,
	)

	if err != nil {
		return 0, fmt.Errorf("failed to create reminder: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get reminder ID: %w", err)
	}

	return id, nil
}

// GetActiveRemindersForUser retrieves all active reminders for a user
func (r *ReminderRepo) GetActiveRemindersForUser(userID int64) ([]models.Reminder, error) {
	query := `
		SELECT id, user_id, message, remind_at, created_at, updated_at,
		       is_fired, is_cancelled, timezone,
		       is_recurring, recurrence_type, recurrence_interval,
		       recurrence_end_at, last_fired_at
		FROM reminders
		WHERE user_id = ? AND is_cancelled = false
		ORDER BY remind_at ASC
	`

	rows, err := r.db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query reminders: %w", err)
	}
	defer rows.Close()

	return r.scanReminders(rows)
}

// GetDueReminders fetches reminders that should fire now
func (r *ReminderRepo) GetDueReminders(before time.Time) ([]models.Reminder, error) {
	query := `
		SELECT id, user_id, message, remind_at, created_at, updated_at,
		       is_fired, is_cancelled, timezone,
		       is_recurring, recurrence_type, recurrence_interval,
		       recurrence_end_at, last_fired_at
		FROM reminders
		WHERE remind_at <= ?
		  AND is_fired = false
		  AND is_cancelled = false
		ORDER BY remind_at ASC
		LIMIT 100
	`

	rows, err := r.db.Query(query, before.Unix())
	if err != nil {
		return nil, fmt.Errorf("failed to query due reminders: %w", err)
	}
	defer rows.Close()

	return r.scanReminders(rows)
}

// MarkReminderFired marks a reminder as fired and updates last_fired_at
func (r *ReminderRepo) MarkReminderFired(reminderID int64, firedAt time.Time) error {
	query := `
		UPDATE reminders
		SET is_fired = true,
		    last_fired_at = ?,
		    updated_at = strftime('%s', 'now')
		WHERE id = ?
	`

	_, err := r.db.Exec(query, firedAt.Unix(), reminderID)
	if err != nil {
		return fmt.Errorf("failed to mark reminder as fired: %w", err)
	}

	return nil
}

// ScheduleNextOccurrence creates the next occurrence for a recurring reminder
func (r *ReminderRepo) ScheduleNextOccurrence(original models.Reminder, nextTime time.Time) error {
	query := `
		INSERT INTO reminders (
			user_id, message, remind_at, timezone,
			is_recurring, recurrence_type, recurrence_interval,
			recurrence_end_at, last_fired_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	var recurrenceType sql.NullString
	if original.RecurrenceType != nil {
		recurrenceType = sql.NullString{String: string(*original.RecurrenceType), Valid: true}
	}

	var recurrenceEndAt sql.NullInt64
	if original.RecurrenceEndAt != nil {
		recurrenceEndAt = sql.NullInt64{Int64: original.RecurrenceEndAt.Unix(), Valid: true}
	}

	var lastFiredAt sql.NullInt64
	now := time.Now()
	lastFiredAt = sql.NullInt64{Int64: now.Unix(), Valid: true}

	_, err := r.db.Exec(query,
		original.UserID,
		original.Message,
		nextTime.Unix(),
		original.Timezone,
		true,
		recurrenceType,
		original.RecurrenceInterval,
		recurrenceEndAt,
		lastFiredAt,
	)

	if err != nil {
		return fmt.Errorf("failed to schedule next occurrence: %w", err)
	}

	return nil
}

// CancelReminder marks a reminder as cancelled
func (r *ReminderRepo) CancelReminder(reminderID int64, userID int64) error {
	query := `
		UPDATE reminders
		SET is_cancelled = true,
		    updated_at = strftime('%s', 'now')
		WHERE id = ? AND user_id = ?
	`

	result, err := r.db.Exec(query, reminderID, userID)
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

// GetReminderByID retrieves a specific reminder (for cancellation verification)
func (r *ReminderRepo) GetReminderByID(reminderID int64, userID int64) (*models.Reminder, error) {
	query := `
		SELECT id, user_id, message, remind_at, created_at, updated_at,
		       is_fired, is_cancelled, timezone,
		       is_recurring, recurrence_type, recurrence_interval,
		       recurrence_end_at, last_fired_at
		FROM reminders
		WHERE id = ? AND user_id = ?
	`

	row := r.db.QueryRow(query, reminderID, userID)
	reminder, err := r.scanReminder(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("reminder not found")
		}
		return nil, fmt.Errorf("failed to get reminder: %w", err)
	}

	return &reminder, nil
}

// scanReminders is a helper to scan multiple reminders from rows
func (r *ReminderRepo) scanReminders(rows *sql.Rows) ([]models.Reminder, error) {
	var reminders []models.Reminder

	for rows.Next() {
		var reminder models.Reminder
		var recurrenceType sql.NullString
		var recurrenceEndAt, lastFiredAt sql.NullInt64
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
			&reminder.Timezone,
			&reminder.IsRecurring,
			&recurrenceType,
			&reminder.RecurrenceInterval,
			&recurrenceEndAt,
			&lastFiredAt,
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

		reminders = append(reminders, reminder)
	}

	return reminders, nil
}

// scanReminder is a helper to scan a single reminder from a row
func (r *ReminderRepo) scanReminder(row *sql.Row) (models.Reminder, error) {
	var reminder models.Reminder
	var recurrenceType sql.NullString
	var recurrenceEndAt, lastFiredAt sql.NullInt64
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
		&reminder.Timezone,
		&reminder.IsRecurring,
		&recurrenceType,
		&reminder.RecurrenceInterval,
		&recurrenceEndAt,
		&lastFiredAt,
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

	return reminder, nil
}
