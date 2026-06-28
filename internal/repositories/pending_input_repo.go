package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

const (
	PendingInputStatusPending   = "pending"
	PendingInputStatusAttached  = "attached"
	PendingInputStatusDiscarded = "discarded"
)

type PendingInputRepo struct {
	db *database.DB
}

func NewPendingInputRepo(db *database.DB) *PendingInputRepo {
	return &PendingInputRepo{db: db}
}

type PendingUserInput struct {
	ID              int64
	UserID          int64
	DialogID        int64
	TgMessageID     int64
	Message         llm.Message
	Status          string
	AttachedTraceID *int64
	CreatedAt       int64
	UpdatedAt       int64
}

type InsertPendingInput struct {
	UserID      int64
	DialogID    int64
	TgMessageID int64
	Message     llm.Message
}

func (r *PendingInputRepo) Insert(ctx context.Context, in InsertPendingInput) (int64, error) {
	payload, err := json.Marshal(in.Message)
	if err != nil {
		return 0, fmt.Errorf("marshal pending input: %w", err)
	}
	res, err := r.db.ExecContext(
		ctx,
		`INSERT INTO pending_user_inputs (user_id, dialog_id, tg_message_id, payload)
		 VALUES (?, ?, ?, ?)`,
		in.UserID, in.DialogID, in.TgMessageID, string(payload),
	)
	if err != nil {
		return 0, fmt.Errorf("insert pending input: %w", err)
	}
	return res.LastInsertId()
}

func (r *PendingInputRepo) ListPendingForDialog(ctx context.Context, userID, dialogID int64, limit int) ([]PendingUserInput, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(
		ctx,
		`SELECT id, user_id, dialog_id, tg_message_id, payload, status, attached_trace_id, created_at, updated_at
		 FROM pending_user_inputs
		 WHERE user_id = ? AND dialog_id = ? AND status = ?
		 ORDER BY tg_message_id ASC, id ASC
		 LIMIT ?`,
		userID, dialogID, PendingInputStatusPending, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending inputs: %w", err)
	}
	defer rows.Close()
	return scanPendingInputs(rows)
}

func (r *PendingInputRepo) ListPendingForDialogTx(tx *sql.Tx, userID, dialogID int64, limit int) ([]PendingUserInput, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := tx.Query(
		`SELECT id, user_id, dialog_id, tg_message_id, payload, status, attached_trace_id, created_at, updated_at
		 FROM pending_user_inputs
		 WHERE user_id = ? AND dialog_id = ? AND status = ?
		 ORDER BY tg_message_id ASC, id ASC
		 LIMIT ?`,
		userID, dialogID, PendingInputStatusPending, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending inputs: %w", err)
	}
	defer rows.Close()
	return scanPendingInputs(rows)
}

func (r *PendingInputRepo) MarkAttached(ctx context.Context, id, traceID int64) error {
	_, err := r.db.ExecContext(
		ctx,
		`UPDATE pending_user_inputs
		 SET status = ?, attached_trace_id = ?, updated_at = strftime('%s', 'now')
		 WHERE id = ? AND status = ?`,
		PendingInputStatusAttached, traceID, id, PendingInputStatusPending,
	)
	if err != nil {
		return fmt.Errorf("mark pending input attached: %w", err)
	}
	return nil
}

func (r *PendingInputRepo) MarkAttachedTx(tx *sql.Tx, id, traceID int64) error {
	_, err := tx.Exec(
		`UPDATE pending_user_inputs
		 SET status = ?, attached_trace_id = ?, updated_at = strftime('%s', 'now')
		 WHERE id = ? AND status = ?`,
		PendingInputStatusAttached, traceID, id, PendingInputStatusPending,
	)
	if err != nil {
		return fmt.Errorf("mark pending input attached: %w", err)
	}
	return nil
}

func (r *PendingInputRepo) DiscardForDialog(ctx context.Context, userID, dialogID int64) error {
	_, err := r.db.ExecContext(
		ctx,
		`UPDATE pending_user_inputs
		 SET status = ?, updated_at = strftime('%s', 'now')
		 WHERE user_id = ? AND dialog_id = ? AND status = ?`,
		PendingInputStatusDiscarded, userID, dialogID, PendingInputStatusPending,
	)
	if err != nil {
		return fmt.Errorf("discard pending inputs: %w", err)
	}
	return nil
}

func scanPendingInputs(rows *sql.Rows) ([]PendingUserInput, error) {
	var out []PendingUserInput
	for rows.Next() {
		var input PendingUserInput
		var payload string
		var attachedTraceID sql.NullInt64
		if err := rows.Scan(
			&input.ID,
			&input.UserID,
			&input.DialogID,
			&input.TgMessageID,
			&payload,
			&input.Status,
			&attachedTraceID,
			&input.CreatedAt,
			&input.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan pending input: %w", err)
		}
		if err := json.Unmarshal([]byte(payload), &input.Message); err != nil {
			return nil, fmt.Errorf("parse pending input payload: %w", err)
		}
		if attachedTraceID.Valid {
			v := attachedTraceID.Int64
			input.AttachedTraceID = &v
		}
		out = append(out, input)
	}
	return out, rows.Err()
}
