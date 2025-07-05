package database

import (
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

	return &DB{db}, nil
}

func (db *DB) Migrate() error {
	slog.Info("Running database migrations")

	migrations := []string{
		createUsersTable,
		createInteractionsTable,
		createMessagesTable,
		createMemoriesTable,
	}

	for i, migration := range migrations {
		if _, err := db.Exec(migration); err != nil {
			return fmt.Errorf("failed to run migration %d: %w", i+1, err)
		}
	}

	slog.Info("Database migrations completed successfully")
	return nil
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

const createInteractionsTable = `
CREATE TABLE IF NOT EXISTS interactions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	author_id INTEGER NOT NULL,
	dialog_id INTEGER NOT NULL,
	tg_user_message_id INTEGER NOT NULL,
	tg_assistant_message_id INTEGER NOT NULL,
	created_at INTEGER DEFAULT (strftime('%s', 'now')),
	FOREIGN KEY (author_id) REFERENCES users(id)
);`

const createMessagesTable = `
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	interaction_id INTEGER NOT NULL,
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL,
	multi_content TEXT, -- JSON for multi-content messages
	created_at INTEGER DEFAULT (strftime('%s', 'now')),
	FOREIGN KEY (interaction_id) REFERENCES interactions(id)
);`

const createMemoriesTable = `
CREATE TABLE IF NOT EXISTS memories (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL,
	memory_key TEXT NOT NULL,
	memory_value TEXT NOT NULL,
	created_at INTEGER DEFAULT (strftime('%s', 'now')),
	updated_at INTEGER DEFAULT (strftime('%s', 'now')),
	FOREIGN KEY (user_id) REFERENCES users(id),
	UNIQUE(user_id, memory_key)
);`
