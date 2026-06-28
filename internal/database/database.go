package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

func NewDB(dbPath string) (*DB, error) {
	err := os.MkdirAll(filepath.Dir(dbPath), 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err = db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, fmt.Errorf("failed to set %s: %w", pragma, err)
		}
	}

	return &DB{db}, nil
}

func (db *DB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) Migrate() error {
	slog.Info("Running database migrations")

	schemaMigrations := []string{
		createUsersTable,
		createTraceEventsTable,
		createPendingUserInputsTable,
		createPreferenceMemoryTable,
		createFactMemoryTable,
		createFactMemoryFTS,
		createFactMemoryFTSTriggers,
		createEpisodicMemoryTable,
		createEpisodicMemoryFTS,
		createEpisodicMemoryFTSTriggers,
		createRemindersTable,
	}

	for i, migration := range schemaMigrations {
		if _, err := db.Exec(migration); err != nil {
			return fmt.Errorf("failed to run schema migration %d: %w", i+1, err)
		}
	}

	if err := db.migrateLegacyTables(); err != nil {
		return fmt.Errorf("failed to migrate legacy tables: %w", err)
	}

	if err := db.dropRemindersTimezoneIfExists(); err != nil {
		return fmt.Errorf("failed to drop reminders.timezone column: %w", err)
	}

	if err := db.addReminderActionColumnsIfMissing(); err != nil {
		return fmt.Errorf("failed to add reminder action columns: %w", err)
	}

	slog.Info("Database migrations completed successfully")
	return nil
}

func (db *DB) addReminderActionColumnsIfMissing() error {
	columns := []struct {
		name string
		sql  string
	}{
		{"is_processing", `ALTER TABLE reminders ADD COLUMN is_processing BOOLEAN DEFAULT false`},
		{"processing_started_at", `ALTER TABLE reminders ADD COLUMN processing_started_at INTEGER`},
		{"action_type", `ALTER TABLE reminders ADD COLUMN action_type TEXT NOT NULL DEFAULT 'notify'`},
		{"action_prompt", `ALTER TABLE reminders ADD COLUMN action_prompt TEXT`},
	}
	for _, column := range columns {
		has, err := columnExists(db.DB, "reminders", column.name)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		slog.Info("Adding reminders column", "column", column.name)
		if _, err := db.Exec(column.sql); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) dropRemindersTimezoneIfExists() error {
	has, err := columnExists(db.DB, "reminders", "timezone")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	slog.Info("Dropping reminders.timezone column (moved to preferences)")
	_, err = db.Exec(`ALTER TABLE reminders DROP COLUMN timezone`)
	return err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (db *DB) migrateLegacyTables() error {
	if exists, err := tableExists(db.DB, "interactions"); err != nil {
		return err
	} else if exists {
		slog.Info("Migrating legacy interactions/messages → trace_events")
		if err := db.migrateInteractionsToTraceEvents(); err != nil {
			return err
		}
	}

	if exists, err := tableExists(db.DB, "memories"); err != nil {
		return err
	} else if exists {
		slog.Info("Migrating legacy memories → preference_memory")
		if err := db.migrateMemoriesToPreferences(); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) migrateInteractionsToTraceEvents() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Each interaction becomes two trace events: user_msg then model_msg.
	// turn_index is monotonic per (user_id, dialog_id), ordered by original created_at.
	migrateSQL := `
		INSERT INTO trace_events (user_id, dialog_id, turn_index, event_type, payload, tg_message_id, created_at)
		SELECT
			i.author_id,
			i.dialog_id,
			(ROW_NUMBER() OVER (PARTITION BY i.author_id, i.dialog_id ORDER BY i.created_at, i.id) - 1) * 2 + offset_role.idx,
			offset_role.role,
			json_object(
				'content', COALESCE(m.content, ''),
				'multi_content', CASE WHEN m.multi_content IS NULL THEN NULL ELSE json(m.multi_content) END
			),
			CASE offset_role.role
				WHEN 'user_msg'  THEN i.tg_user_message_id
				WHEN 'model_msg' THEN i.tg_assistant_message_id
			END,
			i.created_at
		FROM interactions i
		JOIN messages m ON m.interaction_id = i.id
		JOIN (
			SELECT 'user' AS db_role, 'user_msg' AS role, 0 AS idx
			UNION ALL
			SELECT 'assistant', 'model_msg', 1
		) offset_role ON offset_role.db_role = m.role
	`
	if _, err := tx.Exec(migrateSQL); err != nil {
		return fmt.Errorf("copy interactions: %w", err)
	}

	for _, q := range []string{
		"DROP TABLE messages",
		"DROP TABLE interactions",
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}

	return tx.Commit()
}

func (db *DB) migrateMemoriesToPreferences() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO preference_memory (user_id, pref_key, pref_value, source, created_at, updated_at)
		SELECT user_id, memory_key, memory_value, 'explicit', created_at, updated_at FROM memories
	`); err != nil {
		return fmt.Errorf("copy memories: %w", err)
	}

	if _, err := tx.Exec("DROP TABLE memories"); err != nil {
		return fmt.Errorf("drop memories: %w", err)
	}

	return tx.Commit()
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?",
		name,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

const createUsersTable = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	first_name TEXT NOT NULL,
	last_name TEXT,
	username TEXT,
	chat_id INTEGER NOT NULL,
	transcribed_seconds INTEGER DEFAULT 0,
	number_of_input_tokens INTEGER DEFAULT 0,
	number_of_output_tokens INTEGER DEFAULT 0,
	current_dialog_id INTEGER DEFAULT 0,
	last_interaction INTEGER NOT NULL,
	active BOOLEAN DEFAULT true,
	current_model TEXT NOT NULL,
	created_at INTEGER DEFAULT (strftime('%s', 'now')),
	updated_at INTEGER DEFAULT (strftime('%s', 'now'))
);`

const createTraceEventsTable = `
CREATE TABLE IF NOT EXISTS trace_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL,
	dialog_id INTEGER NOT NULL,
	turn_index INTEGER NOT NULL,
	event_type TEXT NOT NULL CHECK (event_type IN ('user_msg','model_msg','tool_call','tool_result')),
	payload TEXT NOT NULL,
	tg_message_id INTEGER,
	model TEXT,
	created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
	FOREIGN KEY (user_id) REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS idx_trace_user_dialog ON trace_events(user_id, dialog_id, turn_index);
CREATE INDEX IF NOT EXISTS idx_trace_user_created ON trace_events(user_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_trace_turn_unique ON trace_events(user_id, dialog_id, turn_index);
`

const createPendingUserInputsTable = `
CREATE TABLE IF NOT EXISTS pending_user_inputs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL,
	dialog_id INTEGER NOT NULL,
	tg_message_id INTEGER NOT NULL,
	payload TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','attached','discarded')),
	attached_trace_id INTEGER,
	created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
	updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
	FOREIGN KEY (user_id) REFERENCES users(id),
	FOREIGN KEY (attached_trace_id) REFERENCES trace_events(id),
	UNIQUE(user_id, dialog_id, tg_message_id)
);
CREATE INDEX IF NOT EXISTS idx_pending_user_dialog_status
	ON pending_user_inputs(user_id, dialog_id, status, tg_message_id, id);
`

const createPreferenceMemoryTable = `
CREATE TABLE IF NOT EXISTS preference_memory (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL,
	pref_key TEXT NOT NULL,
	pref_value TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT 'explicit' CHECK (source IN ('explicit','inferred')),
	source_trace_id INTEGER,
	created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
	updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
	UNIQUE(user_id, pref_key),
	FOREIGN KEY (user_id) REFERENCES users(id),
	FOREIGN KEY (source_trace_id) REFERENCES trace_events(id) ON DELETE SET NULL
);`

const createFactMemoryTable = `
CREATE TABLE IF NOT EXISTS fact_memory (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL,
	subject TEXT NOT NULL,
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	confidence REAL NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('active','superseded','revoked')),
	supersedes_id INTEGER,
	source_trace_id INTEGER NOT NULL,
	embedding BLOB NOT NULL,
	embedding_model TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
	UNIQUE(user_id, content_hash),
	FOREIGN KEY (user_id) REFERENCES users(id),
	FOREIGN KEY (supersedes_id) REFERENCES fact_memory(id),
	FOREIGN KEY (source_trace_id) REFERENCES trace_events(id)
);
CREATE INDEX IF NOT EXISTS idx_fact_user_status ON fact_memory(user_id, status);
CREATE INDEX IF NOT EXISTS idx_fact_user_subject ON fact_memory(user_id, subject);
`

const createFactMemoryFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS fact_memory_fts USING fts5(
	content,
	subject,
	content='fact_memory',
	content_rowid='id'
);`

const createFactMemoryFTSTriggers = `
CREATE TRIGGER IF NOT EXISTS fact_memory_ai AFTER INSERT ON fact_memory BEGIN
	INSERT INTO fact_memory_fts(rowid, content, subject) VALUES (new.id, new.content, new.subject);
END;
CREATE TRIGGER IF NOT EXISTS fact_memory_ad AFTER DELETE ON fact_memory BEGIN
	INSERT INTO fact_memory_fts(fact_memory_fts, rowid, content, subject) VALUES('delete', old.id, old.content, old.subject);
END;
CREATE TRIGGER IF NOT EXISTS fact_memory_au AFTER UPDATE ON fact_memory BEGIN
	INSERT INTO fact_memory_fts(fact_memory_fts, rowid, content, subject) VALUES('delete', old.id, old.content, old.subject);
	INSERT INTO fact_memory_fts(rowid, content, subject) VALUES (new.id, new.content, new.subject);
END;
`

const createEpisodicMemoryTable = `
CREATE TABLE IF NOT EXISTS episodic_memory (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL,
	dialog_id INTEGER NOT NULL,
	summary TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	ended_at INTEGER NOT NULL,
	turn_count INTEGER NOT NULL,
	embedding BLOB NOT NULL,
	embedding_model TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
	UNIQUE(user_id, dialog_id),
	FOREIGN KEY (user_id) REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS idx_episode_user ON episodic_memory(user_id);
`

const createEpisodicMemoryFTS = `
CREATE VIRTUAL TABLE IF NOT EXISTS episodic_memory_fts USING fts5(
	summary,
	content='episodic_memory',
	content_rowid='id'
);`

const createEpisodicMemoryFTSTriggers = `
CREATE TRIGGER IF NOT EXISTS episodic_memory_ai AFTER INSERT ON episodic_memory BEGIN
	INSERT INTO episodic_memory_fts(rowid, summary) VALUES (new.id, new.summary);
END;
CREATE TRIGGER IF NOT EXISTS episodic_memory_ad AFTER DELETE ON episodic_memory BEGIN
	INSERT INTO episodic_memory_fts(episodic_memory_fts, rowid, summary) VALUES('delete', old.id, old.summary);
END;
CREATE TRIGGER IF NOT EXISTS episodic_memory_au AFTER UPDATE ON episodic_memory BEGIN
	INSERT INTO episodic_memory_fts(episodic_memory_fts, rowid, summary) VALUES('delete', old.id, old.summary);
	INSERT INTO episodic_memory_fts(rowid, summary) VALUES (new.id, new.summary);
END;
`

const createRemindersTable = `
CREATE TABLE IF NOT EXISTS reminders (
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
);

CREATE INDEX IF NOT EXISTS idx_reminders_active ON reminders(remind_at, is_fired, is_cancelled)
WHERE is_fired = false AND is_cancelled = false;

CREATE INDEX IF NOT EXISTS idx_reminders_user ON reminders(user_id, is_cancelled);
`
