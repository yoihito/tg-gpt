package services

import (
	"context"
	"log/slog"

	"vadimgribanov.com/tg-gpt/internal/repositories"
)

// WindowCompactor keeps Retriever's live trace-replay window from growing without
// bound while staying cache-friendly. The naive fix — evict the single oldest turn
// every time a new one arrives past some cap — changes the replayed prefix (and so
// busts a prefix-based provider cache, e.g. OpenAI's automatic caching) on every single
// turn once a dialog exceeds that cap. Instead, WindowCompactor waits until the window
// reaches LowWater+BatchSize turns, then evicts a whole BatchSize-turn batch at once
// back down to LowWater, folding what it evicted into a rolling summary via Summarizer
// rather than dropping it. Between evictions — the vast majority of turns — the window
// is pure append: the exact stable-prefix behavior a prefix-based cache needs.
type WindowCompactor struct {
	trace      *repositories.TraceRepo
	summarizer *Summarizer
	lowWater   int
	batchSize  int
}

func NewWindowCompactor(trace *repositories.TraceRepo, summarizer *Summarizer, lowWater, batchSize int) *WindowCompactor {
	return &WindowCompactor{trace: trace, summarizer: summarizer, lowWater: lowWater, batchSize: batchSize}
}

func (w *WindowCompactor) Name() string { return "window_compactor" }

var _ Plugin = (*WindowCompactor)(nil)
var _ AfterTurnHook = (*WindowCompactor)(nil)

// AfterTurn fires compaction in the background — it only affects what the *next* turn
// in this dialog replays, never the response just produced, so it never adds latency to
// the turn the user is waiting on.
func (w *WindowCompactor) AfterTurn(ctx context.Context, rc *RunContext, finalResponse string) {
	go w.MaybeCompact(context.WithoutCancel(ctx), rc.TurnCtx.UserID, rc.TurnCtx.DialogID)
}

// MaybeCompact evicts the oldest BatchSize turns from the live window into the rolling
// summary once the window exceeds LowWater+BatchSize turns; otherwise it's a no-op.
// Errors are logged and swallowed — a stale/missing compaction state just means the
// next turn replays a bit more history than ideal, never something worth failing a
// turn over.
func (w *WindowCompactor) MaybeCompact(ctx context.Context, userID, dialogID int64) {
	if w.lowWater <= 0 || w.batchSize <= 0 {
		return
	}

	windowStart, prevSummary, err := w.trace.GetCompactionState(userID, dialogID)
	if err != nil {
		slog.WarnContext(ctx, "WindowCompactor: read compaction state failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}

	turnsInWindow, err := w.trace.CountUserTurns(userID, dialogID, windowStart)
	if err != nil {
		slog.WarnContext(ctx, "WindowCompactor: count turns failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}
	if turnsInWindow <= int64(w.lowWater+w.batchSize) {
		return
	}

	newWindowStart, ok, err := w.trace.FindNthUserMsgTurnIndex(userID, dialogID, windowStart, w.batchSize)
	if err != nil {
		slog.WarnContext(ctx, "WindowCompactor: find eviction boundary failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}
	if !ok {
		return
	}

	evicted, err := w.trace.GetEventsInRange(userID, dialogID, windowStart, newWindowStart)
	if err != nil {
		slog.WarnContext(ctx, "WindowCompactor: read evicted events failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}
	if len(evicted) == 0 {
		return
	}

	newSummary, err := w.summarizer.Compact(ctx, prevSummary, evicted)
	if err != nil {
		slog.WarnContext(ctx, "WindowCompactor: compaction summary failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}

	if err := w.trace.AdvanceCompaction(userID, dialogID, newWindowStart, newSummary); err != nil {
		slog.WarnContext(ctx, "WindowCompactor: advance compaction failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}
	slog.InfoContext(ctx, "Dialog window compacted",
		"user_id", userID, "dialog_id", dialogID,
		"evicted_turns", w.batchSize, "new_window_start", newWindowStart,
	)
}
