package repositories

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/vec"
)

type FactRepo struct {
	db *database.DB
}

func NewFactRepo(db *database.DB) *FactRepo {
	return &FactRepo{db: db}
}

type InsertFactInput struct {
	UserID         int64
	Subject        string
	Content        string
	ContentHash    string
	Confidence     float64
	Status         string
	SupersedesID   *int64
	SourceTraceID  int64
	Embedding      []float32
	EmbeddingModel string
}

func (r *FactRepo) Insert(in InsertFactInput) (int64, error) {
	var supersedes sql.NullInt64
	if in.SupersedesID != nil {
		supersedes = sql.NullInt64{Int64: *in.SupersedesID, Valid: true}
	}
	res, err := r.db.Exec(`
		INSERT INTO fact_memory
		(user_id, subject, content, content_hash, confidence, status,
		 supersedes_id, source_trace_id, embedding, embedding_model, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		in.UserID, in.Subject, in.Content, in.ContentHash, in.Confidence, in.Status,
		supersedes, in.SourceTraceID, vec.Encode(in.Embedding), in.EmbeddingModel,
		time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("insert fact: %w", err)
	}
	return res.LastInsertId()
}

func (r *FactRepo) GetByContentHash(userID int64, hash string) (*models.Fact, error) {
	row := r.db.QueryRow(`
		SELECT id, user_id, subject, content, content_hash, confidence, status,
		       supersedes_id, source_trace_id, embedding, embedding_model, created_at
		FROM fact_memory
		WHERE user_id = ? AND content_hash = ?
	`, userID, hash)
	return scanFact(row)
}

// ListActiveBySubject returns all active facts for a (user, subject). Used by the
// promotion gate for semantic-dedup and supersession.
func (r *FactRepo) ListActiveBySubject(userID int64, subject string) ([]models.Fact, error) {
	rows, err := r.db.Query(`
		SELECT id, user_id, subject, content, content_hash, confidence, status,
		       supersedes_id, source_trace_id, embedding, embedding_model, created_at
		FROM fact_memory
		WHERE user_id = ? AND subject = ? AND status = 'active'
	`, userID, subject)
	if err != nil {
		return nil, fmt.Errorf("list active by subject: %w", err)
	}
	defer rows.Close()
	return scanFacts(rows)
}

// ListActive returns all active facts for a user. Used by the vector branch of retrieval.
func (r *FactRepo) ListActive(userID int64) ([]models.Fact, error) {
	rows, err := r.db.Query(`
		SELECT id, user_id, subject, content, content_hash, confidence, status,
		       supersedes_id, source_trace_id, embedding, embedding_model, created_at
		FROM fact_memory
		WHERE user_id = ? AND status = 'active'
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list active: %w", err)
	}
	defer rows.Close()
	return scanFacts(rows)
}

// SearchLexical runs an FTS5 MATCH against fact_memory_fts, returning fact ids in BM25
// rank order (best first), filtered to active facts for the given user.
func (r *FactRepo) SearchLexical(userID int64, query string, limit int) ([]int64, error) {
	matchExpr := buildFTS5Query(query)
	if matchExpr == "" {
		return nil, nil
	}
	rows, err := r.db.Query(`
		SELECT f.id
		FROM fact_memory_fts fts
		JOIN fact_memory f ON f.id = fts.rowid
		WHERE fact_memory_fts MATCH ?
		  AND f.user_id = ?
		  AND f.status = 'active'
		ORDER BY bm25(fact_memory_fts) ASC
		LIMIT ?
	`, matchExpr, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
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

func (r *FactRepo) GetByID(id int64) (*models.Fact, error) {
	row := r.db.QueryRow(`
		SELECT id, user_id, subject, content, content_hash, confidence, status,
		       supersedes_id, source_trace_id, embedding, embedding_model, created_at
		FROM fact_memory
		WHERE id = ?
	`, id)
	return scanFact(row)
}

func (r *FactRepo) GetByIDs(ids []int64) ([]models.Fact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `SELECT id, user_id, subject, content, content_hash, confidence, status,
	             supersedes_id, source_trace_id, embedding, embedding_model, created_at
	      FROM fact_memory
	      WHERE id IN (` + placeholders + `)`
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("get facts by ids: %w", err)
	}
	defer rows.Close()
	return scanFacts(rows)
}

func (r *FactRepo) MarkSuperseded(id int64) error {
	_, err := r.db.Exec(`UPDATE fact_memory SET status = 'superseded' WHERE id = ?`, id)
	return err
}

func (r *FactRepo) MarkRevokedBySubject(userID int64, subject string) (int64, error) {
	res, err := r.db.Exec(`
		UPDATE fact_memory SET status = 'revoked'
		WHERE user_id = ? AND subject = ? AND status = 'active'
	`, userID, subject)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *FactRepo) DeleteAllForUser(userID int64) error {
	_, err := r.db.Exec(`DELETE FROM fact_memory WHERE user_id = ?`, userID)
	return err
}

// buildFTS5Query produces an FTS5 MATCH expression from a free-text user query.
// Tokens are extracted as runs of letters/digits, lowercased, wrapped in double quotes,
// and joined with OR. Returns "" if no usable tokens.
func buildFTS5Query(query string) string {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		t := strings.ToLower(current.String())
		current.Reset()
		if len(t) < 2 || isStopword(t) {
			return
		}
		tokens = append(tokens, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " OR ")
}

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"to": true, "of": true, "in": true, "on": true, "at": true, "for": true,
	"with": true, "by": true, "from": true, "as": true, "it": true, "this": true,
	"that": true, "these": true, "those": true, "i": true, "you": true, "we": true,
	"they": true, "he": true, "she": true, "me": true, "my": true, "your": true,
	"do": true, "did": true, "does": true, "what": true, "who": true, "when": true,
	"where": true, "why": true, "how": true, "if": true,
}

func isStopword(t string) bool { return stopwords[t] }

type factScanner interface {
	Scan(dest ...any) error
}

func scanFact(row factScanner) (*models.Fact, error) {
	var f models.Fact
	var supersedes sql.NullInt64
	var embBytes []byte
	err := row.Scan(
		&f.ID, &f.UserID, &f.Subject, &f.Content, &f.ContentHash, &f.Confidence,
		&f.Status, &supersedes, &f.SourceTraceID, &embBytes, &f.EmbeddingModel, &f.CreatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan fact: %w", err)
	}
	if supersedes.Valid {
		v := supersedes.Int64
		f.SupersedesID = &v
	}
	emb, err := vec.Decode(embBytes)
	if err != nil {
		return nil, err
	}
	f.Embedding = emb
	return &f, nil
}

func scanFacts(rows *sql.Rows) ([]models.Fact, error) {
	var out []models.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out = append(out, *f)
		}
	}
	return out, rows.Err()
}
