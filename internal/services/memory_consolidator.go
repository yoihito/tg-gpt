package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/vec"
)

// ConsolidationConfig gates what a MemoryConsolidator is willing to promote or dedup.
type ConsolidationConfig struct {
	FactConfidenceMin   float64
	PrefConfidenceMin   float64
	SemanticDedupCosine float64
}

// MemoryConsolidator turns a finished turn (or an explicit save_memory/save_fact tool
// call) into durable preferences and facts: it runs extraction, applies the confidence
// gate, dedups semantically against what's already stored, and persists what survives.
// It owns no trace-journal or retrieval logic — see TraceStore and Retriever for those.
type MemoryConsolidator struct {
	prefs     *repositories.PreferenceRepo
	facts     *repositories.FactRepo
	embedder  *Embedder
	extractor *Extractor
	cfg       ConsolidationConfig
}

func NewMemoryConsolidator(
	prefs *repositories.PreferenceRepo,
	facts *repositories.FactRepo,
	embedder *Embedder,
	extractor *Extractor,
	cfg ConsolidationConfig,
) *MemoryConsolidator {
	return &MemoryConsolidator{
		prefs:     prefs,
		facts:     facts,
		embedder:  embedder,
		extractor: extractor,
		cfg:       cfg,
	}
}

// EndTurn runs extraction + promotion gate. Designed to be called in a goroutine after
// the response has been streamed; failures are logged but never returned.
func (c *MemoryConsolidator) EndTurn(ctx context.Context, mctx TurnContext, userMsg, assistantMsg string) {
	if userMsg == "" && assistantMsg == "" {
		return
	}
	candidates, err := c.extractor.Extract(ctx, ExtractInput{
		UserMessage:      userMsg,
		AssistantMessage: assistantMsg,
	})
	if err != nil {
		slog.WarnContext(ctx, "extractor failed", "error", err)
		return
	}
	for _, cand := range candidates {
		if err := c.promote(ctx, mctx, cand); err != nil {
			slog.WarnContext(ctx, "promotion failed", "error", err, "type", cand.Type)
		}
	}
}

// PromoteExplicit runs the promotion gate for a caller-provided candidate (e.g. the
// LLM explicitly invoking save_memory or save_fact). The gate still enforces dedup,
// but confidence defaults to 1.0 when unset.
func (c *MemoryConsolidator) PromoteExplicit(ctx context.Context, mctx TurnContext, cand Candidate) error {
	if cand.Confidence == 0 {
		cand.Confidence = 1.0
	}
	return c.promote(ctx, mctx, cand)
}

// RevokeFactsBySubject marks all active facts for (user, subject) as revoked.
func (c *MemoryConsolidator) RevokeFactsBySubject(userID int64, subject string) (int64, error) {
	return c.facts.MarkRevokedBySubject(userID, subject)
}

func (c *MemoryConsolidator) promote(ctx context.Context, mctx TurnContext, cand Candidate) error {
	switch cand.Type {
	case CandidatePreference:
		if cand.Confidence < c.cfg.PrefConfidenceMin || cand.Key == "" || cand.Value == "" {
			return nil
		}
		if _, err := ValidatePreference(cand.Key, cand.Value); err != nil {
			slog.WarnContext(ctx, "Dropping invalid inferred preference",
				"key", cand.Key, "value", cand.Value, "error", err)
			return nil
		}
		traceID := mctx.UserTraceID
		return c.prefs.Upsert(repositories.UpsertPreferenceInput{
			UserID:        mctx.UserID,
			Key:           cand.Key,
			Value:         cand.Value,
			Source:        models.PreferenceSourceInferred,
			SourceTraceID: &traceID,
		})

	case CandidateFact:
		if cand.Confidence < c.cfg.FactConfidenceMin || cand.Subject == "" || cand.Content == "" {
			return nil
		}
		hash := contentHash(cand.Content)
		if existing, err := c.facts.GetByContentHash(mctx.UserID, hash); err != nil {
			return fmt.Errorf("hash lookup: %w", err)
		} else if existing != nil {
			return nil
		}

		emb, err := c.embedder.Embed(ctx, cand.Subject+" "+cand.Content)
		if err != nil {
			return fmt.Errorf("embed fact: %w", err)
		}

		sameSubject, err := c.facts.ListActiveBySubject(mctx.UserID, cand.Subject)
		if err != nil {
			return fmt.Errorf("list same subject: %w", err)
		}
		for _, ex := range sameSubject {
			if ex.EmbeddingModel != c.embedder.Model() {
				continue
			}
			if vec.Cosine(emb, ex.Embedding) >= float32(c.cfg.SemanticDedupCosine) {
				return nil
			}
		}

		_, err = c.facts.Insert(repositories.InsertFactInput{
			UserID:         mctx.UserID,
			Subject:        cand.Subject,
			Content:        cand.Content,
			ContentHash:    hash,
			Confidence:     cand.Confidence,
			Status:         models.FactStatusActive,
			SourceTraceID:  mctx.UserTraceID,
			Embedding:      emb,
			EmbeddingModel: c.embedder.Model(),
		})
		if err != nil {
			return fmt.Errorf("insert fact: %w", err)
		}
		return nil
	}
	return nil
}

func contentHash(s string) string {
	h := sha256.Sum256([]byte(normalizeContent(s)))
	return hex.EncodeToString(h[:])
}

func normalizeContent(s string) string {
	s = strings.ToLower(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}
