package database

import (
	"path/filepath"
	"testing"
)

// TestAddReminderDialogIDAndHourlyRecurrenceIfMissing exercises the SQLite table-rebuild
// path directly against a pre-migration ("old-shaped") reminders table, since Migrate()
// alone would never hit it on a fresh test DB — CREATE TABLE IF NOT EXISTS already
// creates the new shape there.
func TestAddReminderDialogIDAndHourlyRecurrenceIfMissing(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (id) VALUES (42)`); err != nil {
		t.Fatal(err)
	}

	oldDDL := `CREATE TABLE reminders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		message TEXT NOT NULL,
		remind_at INTEGER NOT NULL,
		created_at INTEGER DEFAULT (strftime('%s', 'now')),
		updated_at INTEGER DEFAULT (strftime('%s', 'now')),
		is_fired BOOLEAN DEFAULT false,
		is_cancelled BOOLEAN DEFAULT false,
		is_processing BOOLEAN DEFAULT false,
		is_recurring BOOLEAN DEFAULT false,
		recurrence_type TEXT CHECK (recurrence_type IN ('daily', 'weekly', 'monthly', NULL)),
		recurrence_interval INTEGER DEFAULT 1,
		recurrence_end_at INTEGER,
		last_fired_at INTEGER,
		processing_started_at INTEGER,
		action_type TEXT NOT NULL DEFAULT 'notify',
		action_prompt TEXT,
		FOREIGN KEY (user_id) REFERENCES users(id)
	)`
	if _, err := db.Exec(oldDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO reminders (id, user_id, message, remind_at, recurrence_type) VALUES (1, 42, 'water plants', 1700000000, 'weekly')`,
	); err != nil {
		t.Fatal(err)
	}

	if err := db.addReminderDialogIDAndHourlyRecurrenceIfMissing(); err != nil {
		t.Fatal(err)
	}

	var dialogID int64
	var message, recurrenceType string
	if err := db.QueryRow(`SELECT dialog_id, message, recurrence_type FROM reminders WHERE id = 1`).
		Scan(&dialogID, &message, &recurrenceType); err != nil {
		t.Fatal(err)
	}
	if dialogID != 0 {
		t.Fatalf("dialog_id: got %d want 0 (existing rows default to General)", dialogID)
	}
	if message != "water plants" {
		t.Fatalf("message lost across rebuild: got %q", message)
	}
	if recurrenceType != "weekly" {
		t.Fatalf("recurrence_type lost across rebuild: got %q", recurrenceType)
	}

	if _, err := db.Exec(
		`INSERT INTO reminders (id, user_id, dialog_id, message, remind_at, recurrence_type) VALUES (2, 42, 5, 'check news', 1700000000, 'hourly')`,
	); err != nil {
		t.Fatalf("'hourly' should be accepted by the widened CHECK constraint: %v", err)
	}

	has, err := columnExists(db.DB, "reminders", "dialog_id")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("dialog_id column missing after rebuild")
	}

	// Idempotent: calling it again once already migrated must be a no-op, not an error.
	if err := db.addReminderDialogIDAndHourlyRecurrenceIfMissing(); err != nil {
		t.Fatalf("second call should be a no-op: %v", err)
	}
}
