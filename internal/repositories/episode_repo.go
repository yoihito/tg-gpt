package repositories

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/vec"
)

type EpisodeRepo struct {
	db *database.DB
}

func NewEpisodeRepo(db *database.DB) *EpisodeRepo {
	return &EpisodeRepo{db: db}
}

type InsertEpisodeInput struct {
	UserID         int64
	DialogID       int64
	Summary        string
	StartedAt      int64
	EndedAt        int64
	TurnCount      int64
	Embedding      []float32
	EmbeddingModel string
}

func (r *EpisodeRepo) Insert(in InsertEpisodeInput) (int64, error) {
	res, err := r.db.Exec(`
		INSERT INTO episodic_memory
		(user_id, dialog_id, summary, started_at, ended_at, turn_count, embedding, embedding_model, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		in.UserID, in.DialogID, in.Summary, in.StartedAt, in.EndedAt,
		in.TurnCount, vec.Encode(in.Embedding), in.EmbeddingModel,
		time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("insert episode: %w", err)
	}
	return res.LastInsertId()
}

func (r *EpisodeRepo) ExistsForDialog(userID, dialogID int64) (bool, error) {
	var n int
	err := r.db.QueryRow(
		`SELECT count(*) FROM episodic_memory WHERE user_id = ? AND dialog_id = ?`,
		userID, dialogID,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListAll returns every episode for a user. Used by the vector branch of retrieval —
// single-user data volumes keep linear cosine fast.
func (r *EpisodeRepo) ListAll(userID int64) ([]models.Episode, error) {
	rows, err := r.db.Query(`
		SELECT id, user_id, dialog_id, summary, started_at, ended_at,
		       turn_count, embedding, embedding_model, created_at
		FROM episodic_memory
		WHERE user_id = ?
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list episodes: %w", err)
	}
	defer rows.Close()
	return scanEpisodes(rows)
}

func (r *EpisodeRepo) SearchLexical(userID int64, query string, limit int) ([]int64, error) {
	matchExpr := buildFTS5Query(query)
	if matchExpr == "" {
		return nil, nil
	}
	rows, err := r.db.Query(`
		SELECT e.id
		FROM episodic_memory_fts fts
		JOIN episodic_memory e ON e.id = fts.rowid
		WHERE episodic_memory_fts MATCH ?
		  AND e.user_id = ?
		ORDER BY bm25(episodic_memory_fts) ASC
		LIMIT ?
	`, matchExpr, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("fts episode search: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *EpisodeRepo) GetByIDs(ids []int64) ([]models.Episode, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := r.db.Query(`
		SELECT id, user_id, dialog_id, summary, started_at, ended_at,
		       turn_count, embedding, embedding_model, created_at
		FROM episodic_memory
		WHERE id IN (`+placeholders+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("get episodes by ids: %w", err)
	}
	defer rows.Close()
	return scanEpisodes(rows)
}

func (r *EpisodeRepo) DeleteAllForUser(userID int64) error {
	_, err := r.db.Exec(`DELETE FROM episodic_memory WHERE user_id = ?`, userID)
	return err
}

// Delete removes one episode if it belongs to the given user. Returns an error
// if no row matched, so callers can surface "not found / unauthorized" to the LLM.
func (r *EpisodeRepo) Delete(id, userID int64) error {
	res, err := r.db.Exec(`DELETE FROM episodic_memory WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete episode: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("episode not found or unauthorized")
	}
	return nil
}

type episodeScanner interface {
	Scan(dest ...any) error
}

func scanEpisode(row episodeScanner) (*models.Episode, error) {
	var e models.Episode
	var embBytes []byte
	err := row.Scan(
		&e.ID, &e.UserID, &e.DialogID, &e.Summary, &e.StartedAt, &e.EndedAt,
		&e.TurnCount, &embBytes, &e.EmbeddingModel, &e.CreatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan episode: %w", err)
	}
	emb, err := vec.Decode(embBytes)
	if err != nil {
		return nil, err
	}
	e.Embedding = emb
	return &e, nil
}

func scanEpisodes(rows *sql.Rows) ([]models.Episode, error) {
	var out []models.Episode
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, *e)
		}
	}
	return out, rows.Err()
}
