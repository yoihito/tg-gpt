package services

import (
	"context"
	"log/slog"
	"strings"

	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

// EpisodeConfig controls when a dialog is worth summarizing into an episode.
type EpisodeConfig struct {
	MinTurns int
}

// EpisodeStore owns episodic memory: closing a finished dialog into a summarized
// episode, and listing/deleting episodes on the user's behalf. It owns no trace-journal
// writes or retrieval logic — see TraceStore and Retriever for those (Retriever is what
// actually searches episodes back into a turn's context).
type EpisodeStore struct {
	trace      *repositories.TraceRepo
	episodes   *repositories.EpisodeRepo
	summarizer *Summarizer
	embedder   *Embedder
	cfg        EpisodeConfig
}

func NewEpisodeStore(
	trace *repositories.TraceRepo,
	episodes *repositories.EpisodeRepo,
	summarizer *Summarizer,
	embedder *Embedder,
	cfg EpisodeConfig,
) *EpisodeStore {
	return &EpisodeStore{
		trace:      trace,
		episodes:   episodes,
		summarizer: summarizer,
		embedder:   embedder,
		cfg:        cfg,
	}
}

// ListEpisodes returns all stored episodes for a user (used by the list_episodes tool).
func (e *EpisodeStore) ListEpisodes(userID int64) ([]models.Episode, error) {
	return e.episodes.ListAll(userID)
}

// DeleteEpisode removes one episode after verifying it belongs to the user.
func (e *EpisodeStore) DeleteEpisode(userID, id int64) error {
	return e.episodes.Delete(id, userID)
}

// CloseDialog summarizes the given (user, dialog) and writes one episodic_memory row.
// Idempotent: skips if an episode already exists for that dialog or if fewer than
// EpisodeMinTurns turns are present. Failures are logged but never returned — the
// caller (e.g. /new_chat handler) must not block on this.
func (e *EpisodeStore) CloseDialog(ctx context.Context, userID, dialogID int64) {
	events, err := e.trace.GetAllForDialog(userID, dialogID)
	if err != nil {
		slog.WarnContext(ctx, "CloseDialog: read trace failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}
	var turnCount int64
	var startedAt, endedAt int64
	for _, ev := range events {
		if ev.EventType == models.EventTypeUserMsg || ev.EventType == models.EventTypeModelMsg {
			turnCount++
		}
		if startedAt == 0 || ev.CreatedAt < startedAt {
			startedAt = ev.CreatedAt
		}
		if ev.CreatedAt > endedAt {
			endedAt = ev.CreatedAt
		}
	}
	if turnCount < int64(e.cfg.MinTurns) {
		return
	}

	exists, err := e.episodes.ExistsForDialog(userID, dialogID)
	if err != nil {
		slog.WarnContext(ctx, "CloseDialog: existence check failed", "error", err)
		return
	}
	if exists {
		return
	}

	summary, err := e.summarizer.Summarize(ctx, events)
	if err != nil {
		slog.WarnContext(ctx, "CloseDialog: summarizer failed", "error", err)
		return
	}
	if strings.TrimSpace(summary) == "" {
		return
	}

	emb, err := e.embedder.Embed(ctx, summary)
	if err != nil {
		slog.WarnContext(ctx, "CloseDialog: embed failed", "error", err)
		return
	}

	if _, err := e.episodes.Insert(repositories.InsertEpisodeInput{
		UserID:         userID,
		DialogID:       dialogID,
		Summary:        summary,
		StartedAt:      startedAt,
		EndedAt:        endedAt,
		TurnCount:      turnCount,
		Embedding:      emb,
		EmbeddingModel: e.embedder.Model(),
	}); err != nil {
		slog.WarnContext(ctx, "CloseDialog: insert failed", "error", err)
		return
	}
	slog.InfoContext(ctx, "dialog summarized", "user_id", userID, "dialog_id", dialogID, "turn_count", turnCount)
}
