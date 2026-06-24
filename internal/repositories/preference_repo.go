package repositories

import (
	"database/sql"
	"fmt"
	"time"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type PreferenceRepo struct {
	db *database.DB
}

func NewPreferenceRepo(db *database.DB) *PreferenceRepo {
	return &PreferenceRepo{db: db}
}

type UpsertPreferenceInput struct {
	UserID        int64
	Key           string
	Value         string
	Source        string
	SourceTraceID *int64
}

func (r *PreferenceRepo) Upsert(in UpsertPreferenceInput) error {
	now := time.Now().Unix()
	var traceID sql.NullInt64
	if in.SourceTraceID != nil {
		traceID = sql.NullInt64{Int64: *in.SourceTraceID, Valid: true}
	}
	_, err := r.db.Exec(`
		INSERT INTO preference_memory (user_id, pref_key, pref_value, source, source_trace_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, pref_key) DO UPDATE SET
			pref_value = excluded.pref_value,
			source = excluded.source,
			source_trace_id = excluded.source_trace_id,
			updated_at = excluded.updated_at
	`, in.UserID, in.Key, in.Value, in.Source, traceID, now, now)
	if err != nil {
		return fmt.Errorf("upsert preference: %w", err)
	}
	return nil
}

func (r *PreferenceRepo) Get(userID int64, key string) (*models.Preference, error) {
	var p models.Preference
	var traceID sql.NullInt64
	err := r.db.QueryRow(`
		SELECT id, user_id, pref_key, pref_value, source, source_trace_id, created_at, updated_at
		FROM preference_memory
		WHERE user_id = ? AND pref_key = ?
	`, userID, key).Scan(&p.ID, &p.UserID, &p.PrefKey, &p.PrefValue, &p.Source, &traceID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get preference: %w", err)
	}
	if traceID.Valid {
		v := traceID.Int64
		p.SourceTraceID = &v
	}
	return &p, nil
}

func (r *PreferenceRepo) GetAll(userID int64) ([]models.Preference, error) {
	rows, err := r.db.Query(`
		SELECT id, user_id, pref_key, pref_value, source, source_trace_id, created_at, updated_at
		FROM preference_memory
		WHERE user_id = ?
		ORDER BY pref_key ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("query preferences: %w", err)
	}
	defer rows.Close()

	var out []models.Preference
	for rows.Next() {
		var p models.Preference
		var traceID sql.NullInt64
		if err := rows.Scan(&p.ID, &p.UserID, &p.PrefKey, &p.PrefValue, &p.Source, &traceID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan preference: %w", err)
		}
		if traceID.Valid {
			v := traceID.Int64
			p.SourceTraceID = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *PreferenceRepo) Delete(userID int64, key string) error {
	_, err := r.db.Exec(`DELETE FROM preference_memory WHERE user_id = ? AND pref_key = ?`, userID, key)
	if err != nil {
		return fmt.Errorf("delete preference: %w", err)
	}
	return nil
}

func (r *PreferenceRepo) DeleteAllForUser(userID int64) error {
	_, err := r.db.Exec(`DELETE FROM preference_memory WHERE user_id = ?`, userID)
	return err
}
