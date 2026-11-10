package repositories

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type TraceRepo struct {
	db *database.DB
}

func NewTraceRepo(db *database.DB) *TraceRepo {
	return &TraceRepo{db: db}
}

type AppendEventInput struct {
	UserID      int64
	DialogID    int64
	EventType   string
	Payload     any
	TgMessageID *int64
	Model       string
}

// Append inserts a single trace event with a monotonic turn_index for (user_id, dialog_id).
// Returns the new event's id.
func (r *TraceRepo) Append(in AppendEventInput) (int64, error) {
	payloadBytes, err := json.Marshal(in.Payload)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}

	tx, err := r.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var nextIdx int64
	err = tx.QueryRow(
		`SELECT COALESCE(MAX(turn_index), -1) + 1 FROM trace_events WHERE user_id = ? AND dialog_id = ?`,
		in.UserID, in.DialogID,
	).Scan(&nextIdx)
	if err != nil {
		return 0, fmt.Errorf("compute turn_index: %w", err)
	}

	var tgMsgID sql.NullInt64
	if in.TgMessageID != nil {
		tgMsgID = sql.NullInt64{Int64: *in.TgMessageID, Valid: true}
	}
	var model sql.NullString
	if in.Model != "" {
		model = sql.NullString{String: in.Model, Valid: true}
	}

	res, err := tx.Exec(
		`INSERT INTO trace_events (user_id, dialog_id, turn_index, event_type, payload, tg_message_id, model)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, in.DialogID, nextIdx, in.EventType, string(payloadBytes), tgMsgID, model,
	)
	if err != nil {
		return 0, fmt.Errorf("insert trace_event: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (r *TraceRepo) AppendBatchTx(tx *sql.Tx, userID, dialogID int64, events []AppendEventInput) ([]int64, error) {
	if len(events) == 0 {
		return nil, nil
	}

	var nextIdx int64
	err := tx.QueryRow(
		`SELECT COALESCE(MAX(turn_index), -1) + 1 FROM trace_events WHERE user_id = ? AND dialog_id = ?`,
		userID, dialogID,
	).Scan(&nextIdx)
	if err != nil {
		return nil, fmt.Errorf("compute turn_index: %w", err)
	}

	ids := make([]int64, 0, len(events))
	for i, event := range events {
		event.UserID = userID
		event.DialogID = dialogID
		id, err := appendTraceEventTx(tx, event, nextIdx+int64(i))
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func appendTraceEventTx(tx *sql.Tx, in AppendEventInput, turnIndex int64) (int64, error) {
	payloadBytes, err := json.Marshal(in.Payload)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}

	var tgMsgID sql.NullInt64
	if in.TgMessageID != nil {
		tgMsgID = sql.NullInt64{Int64: *in.TgMessageID, Valid: true}
	}
	var model sql.NullString
	if in.Model != "" {
		model = sql.NullString{String: in.Model, Valid: true}
	}

	res, err := tx.Exec(
		`INSERT INTO trace_events (user_id, dialog_id, turn_index, event_type, payload, tg_message_id, model)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, in.DialogID, turnIndex, in.EventType, string(payloadBytes), tgMsgID, model,
	)
	if err != nil {
		return 0, fmt.Errorf("insert trace_event: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetAllForDialog returns every event for (user_id, dialog_id) in turn order.
// Used by CloseDialog summarization.
func (r *TraceRepo) GetAllForDialog(userID, dialogID int64) ([]models.TraceEvent, error) {
	rows, err := r.db.Query(
		`SELECT id, user_id, dialog_id, turn_index, event_type, payload, tg_message_id, model, created_at
		 FROM trace_events
		 WHERE user_id = ? AND dialog_id = ?
		 ORDER BY turn_index ASC`,
		userID, dialogID,
	)
	if err != nil {
		return nil, fmt.Errorf("query dialog trace: %w", err)
	}
	defer rows.Close()
	return scanTraceEvents(rows)
}

// GetEventsFrom returns every event for (user_id, dialog_id) with turn_index >=
// fromTurnIndex, oldest first. Passing 0 (the default for a dialog with no compaction
// state yet) returns the whole dialog, since turn_index starts at 0.
func (r *TraceRepo) GetEventsFrom(userID, dialogID, fromTurnIndex int64) ([]models.TraceEvent, error) {
	rows, err := r.db.Query(
		`SELECT id, user_id, dialog_id, turn_index, event_type, payload, tg_message_id, model, created_at
		 FROM trace_events WHERE user_id = ? AND dialog_id = ? AND turn_index >= ?
		 ORDER BY turn_index ASC`,
		userID, dialogID, fromTurnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("query events from: %w", err)
	}
	defer rows.Close()
	return scanTraceEvents(rows)
}

// GetEventsInRange returns every event for (user_id, dialog_id) with
// fromTurnIndex <= turn_index < toTurnIndexExclusive, oldest first — the batch a
// compaction pass is about to fold into its rolling summary.
func (r *TraceRepo) GetEventsInRange(userID, dialogID, fromTurnIndex, toTurnIndexExclusive int64) ([]models.TraceEvent, error) {
	rows, err := r.db.Query(
		`SELECT id, user_id, dialog_id, turn_index, event_type, payload, tg_message_id, model, created_at
		 FROM trace_events WHERE user_id = ? AND dialog_id = ? AND turn_index >= ? AND turn_index < ?
		 ORDER BY turn_index ASC`,
		userID, dialogID, fromTurnIndex, toTurnIndexExclusive,
	)
	if err != nil {
		return nil, fmt.Errorf("query events in range: %w", err)
	}
	defer rows.Close()
	return scanTraceEvents(rows)
}

// CountUserTurns counts user_msg events (each one starts a turn) at or after
// fromTurnIndex — i.e. how many turns are currently in the live window.
func (r *TraceRepo) CountUserTurns(userID, dialogID, fromTurnIndex int64) (int64, error) {
	var count int64
	err := r.db.QueryRow(
		`SELECT COUNT(*) FROM trace_events
		 WHERE user_id = ? AND dialog_id = ? AND event_type = ? AND turn_index >= ?`,
		userID, dialogID, models.EventTypeUserMsg, fromTurnIndex,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count user turns: %w", err)
	}
	return count, nil
}

// FindNthUserMsgTurnIndex returns the turn_index of the n-th (0-indexed) user_msg event
// at or after fromTurnIndex. ok is false if the dialog has n or fewer turns from there —
// callers use that to mean "nothing to evict yet".
func (r *TraceRepo) FindNthUserMsgTurnIndex(userID, dialogID, fromTurnIndex int64, n int) (int64, bool, error) {
	var turnIndex int64
	err := r.db.QueryRow(
		`SELECT turn_index FROM trace_events
		 WHERE user_id = ? AND dialog_id = ? AND event_type = ? AND turn_index >= ?
		 ORDER BY turn_index ASC
		 LIMIT 1 OFFSET ?`,
		userID, dialogID, models.EventTypeUserMsg, fromTurnIndex, n,
	).Scan(&turnIndex)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find nth user_msg turn_index: %w", err)
	}
	return turnIndex, true, nil
}

// GetCompactionState returns the live window's start turn_index and the rolling summary
// of whatever was compacted before it, for (user_id, dialog_id). A dialog with no
// compaction row yet (never crossed the high-water mark) returns (0, "", nil) — the
// same as "replay from the beginning".
func (r *TraceRepo) GetCompactionState(userID, dialogID int64) (int64, string, error) {
	var windowStart int64
	var summary string
	err := r.db.QueryRow(
		`SELECT window_start_turn_index, rolling_summary FROM dialog_compaction WHERE user_id = ? AND dialog_id = ?`,
		userID, dialogID,
	).Scan(&windowStart, &summary)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("get compaction state: %w", err)
	}
	return windowStart, summary, nil
}

// AdvanceCompaction records that everything before newWindowStart has been folded into
// newSummary, so future replays start there instead of from the beginning.
func (r *TraceRepo) AdvanceCompaction(userID, dialogID, newWindowStart int64, newSummary string) error {
	_, err := r.db.Exec(
		`INSERT INTO dialog_compaction (user_id, dialog_id, window_start_turn_index, rolling_summary, updated_at)
		 VALUES (?, ?, ?, ?, strftime('%s', 'now'))
		 ON CONFLICT (user_id, dialog_id) DO UPDATE SET
		   window_start_turn_index = excluded.window_start_turn_index,
		   rolling_summary = excluded.rolling_summary,
		   updated_at = excluded.updated_at`,
		userID, dialogID, newWindowStart, newSummary,
	)
	if err != nil {
		return fmt.Errorf("advance compaction: %w", err)
	}
	return nil
}

// PopLatestExchange removes and returns the most recent user_msg event and any subsequent
// events for that user (used by /retry). Returns the user_msg payload so the caller can replay it.
func (r *TraceRepo) PopLatestExchange(userID, dialogID int64) (models.TraceEvent, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return models.TraceEvent{}, err
	}
	defer tx.Rollback()

	var userMsg models.TraceEvent
	var tgMsgID sql.NullInt64
	var model sql.NullString
	var payload string
	err = tx.QueryRow(
		`SELECT id, user_id, dialog_id, turn_index, event_type, payload, tg_message_id, model, created_at
		 FROM trace_events
		 WHERE user_id = ? AND dialog_id = ? AND event_type = ?
		 ORDER BY turn_index DESC
		 LIMIT 1`,
		userID, dialogID, models.EventTypeUserMsg,
	).Scan(
		&userMsg.ID, &userMsg.UserID, &userMsg.DialogID, &userMsg.TurnIndex,
		&userMsg.EventType, &payload, &tgMsgID, &model, &userMsg.CreatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return models.TraceEvent{}, fmt.Errorf("no user message to retry")
		}
		return models.TraceEvent{}, fmt.Errorf("find latest user_msg: %w", err)
	}
	userMsg.Payload = json.RawMessage(payload)
	if tgMsgID.Valid {
		v := tgMsgID.Int64
		userMsg.TgMessageID = &v
	}
	if model.Valid {
		userMsg.Model = model.String
	}

	_, err = tx.Exec(
		`DELETE FROM trace_events
		 WHERE user_id = ? AND dialog_id = ? AND turn_index >= ?`,
		userID, dialogID, userMsg.TurnIndex,
	)
	if err != nil {
		return models.TraceEvent{}, fmt.Errorf("delete tail: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return models.TraceEvent{}, err
	}
	return userMsg, nil
}

func (r *TraceRepo) DeleteAllForUser(userID int64) error {
	_, err := r.db.Exec(`DELETE FROM trace_events WHERE user_id = ?`, userID)
	return err
}

func scanTraceEvents(rows *sql.Rows) ([]models.TraceEvent, error) {
	var out []models.TraceEvent
	for rows.Next() {
		var e models.TraceEvent
		var tgMsgID sql.NullInt64
		var model sql.NullString
		var payload string
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.DialogID, &e.TurnIndex,
			&e.EventType, &payload, &tgMsgID, &model, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan trace_event: %w", err)
		}
		e.Payload = json.RawMessage(payload)
		if tgMsgID.Valid {
			v := tgMsgID.Int64
			e.TgMessageID = &v
		}
		if model.Valid {
			e.Model = model.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
