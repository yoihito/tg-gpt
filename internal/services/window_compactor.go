package services

import (
	"context"
	"encoding/json"
	"log/slog"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

// WindowCompactor keeps Retriever's live trace-replay window from growing without
// bound while staying cache-friendly. The naive fix — evict the single oldest turn
// every time a new one arrives past some cap — changes the replayed prefix (and so
// busts a prefix-based provider cache, e.g. OpenAI's automatic caching) on every single
// turn once a dialog exceeds that cap. Instead, WindowCompactor waits until the window's
// real size (the provider-reported InputTokens for the turn that just ran) crosses
// HighWaterTokens, then evicts however many of the oldest turns are needed to bring the
// window's own content back down to roughly LowWaterTokens, folding what it evicted into
// a rolling summary via Summarizer rather than dropping it. Between evictions — the vast
// majority of turns — the window is pure append: the exact stable-prefix behavior a
// prefix-based cache needs.
type WindowCompactor struct {
	trace           *repositories.TraceRepo
	summarizer      *Summarizer
	lowWaterTokens  int64
	highWaterTokens int64
}

func NewWindowCompactor(trace *repositories.TraceRepo, summarizer *Summarizer, lowWaterTokens, highWaterTokens int) *WindowCompactor {
	return &WindowCompactor{
		trace:           trace,
		summarizer:      summarizer,
		lowWaterTokens:  int64(lowWaterTokens),
		highWaterTokens: int64(highWaterTokens),
	}
}

func (w *WindowCompactor) Name() string { return "window_compactor" }

var _ Plugin = (*WindowCompactor)(nil)
var _ AfterModelCallHook = (*WindowCompactor)(nil)
var _ AfterTurnHook = (*WindowCompactor)(nil)

// AfterModelCall records the real prompt size the provider just reported, so AfterTurn
// has it once the turn (and all its iterations) are done. Later iterations overwrite
// earlier ones on purpose: by the time the turn ends, this holds the size of the request
// that produced the final response — the most representative measurement of how big
// this dialog's context currently is.
func (w *WindowCompactor) AfterModelCall(ctx context.Context, rc *RunContext, content string, toolCalls []llm.ToolCall, usage llm.Usage) error {
	rc.LastInputTokens = usage.InputTokens
	return nil
}

// AfterTurn fires compaction in the background — it only affects what the *next* turn
// in this dialog replays, never the response just produced, so it never adds latency to
// the turn the user is waiting on.
func (w *WindowCompactor) AfterTurn(ctx context.Context, rc *RunContext, finalResponse string) {
	go w.MaybeCompact(context.WithoutCancel(ctx), rc.TurnCtx.UserID, rc.TurnCtx.DialogID, rc.LastInputTokens)
}

// MaybeCompact evicts the oldest turns from the live window — enough to bring the
// window's own (estimated) size back down to roughly LowWaterTokens, always keeping at
// least the most recent turn — once currentInputTokens (the real size of the turn that
// just ran) exceeds HighWaterTokens; otherwise it's a no-op. Errors are logged and
// swallowed — a stale/missing compaction state just means the next turn replays a bit
// more history than ideal, never something worth failing a turn over.
func (w *WindowCompactor) MaybeCompact(ctx context.Context, userID, dialogID, currentInputTokens int64) {
	if w.highWaterTokens <= 0 || currentInputTokens <= w.highWaterTokens {
		return
	}

	windowStart, prevSummary, err := w.trace.GetCompactionState(userID, dialogID)
	if err != nil {
		slog.WarnContext(ctx, "WindowCompactor: read compaction state failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}

	events, err := w.trace.GetEventsFrom(userID, dialogID, windowStart)
	if err != nil {
		slog.WarnContext(ctx, "WindowCompactor: read window events failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}
	if len(events) == 0 {
		return
	}

	groups, startIdx := groupEventsByTurn(events)
	if len(groups) <= 1 {
		// Nothing to evict while still keeping at least the most recent turn.
		return
	}

	var total int64
	for _, g := range groups {
		total += g.tokens
	}
	if total <= w.lowWaterTokens {
		// The window's own content is already small — whatever pushed
		// currentInputTokens over the high-water mark lives elsewhere (system
		// prompt, preferences, rolling summary), and evicting more live turns
		// wouldn't help.
		return
	}

	remaining := total
	boundary := 0
	for boundary < len(groups)-1 && remaining > w.lowWaterTokens {
		remaining -= groups[boundary].tokens
		boundary++
	}

	newWindowStart := groups[boundary].turnIndex
	evicted := events[:startIdx[boundary]]
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
		"evicted_turns", boundary, "evicted_tokens_est", total-remaining, "new_window_start", newWindowStart,
	)
}

type turnGroup struct {
	turnIndex int64
	tokens    int64
}

// groupEventsByTurn splits events (already ordered oldest-first) into per-turn groups,
// each starting at a user_msg event, alongside each group's start index into events —
// so the caller can slice events directly instead of issuing a second query.
func groupEventsByTurn(events []models.TraceEvent) ([]turnGroup, []int) {
	var groups []turnGroup
	var startIdx []int
	for i, e := range events {
		if e.EventType == models.EventTypeUserMsg {
			groups = append(groups, turnGroup{turnIndex: e.TurnIndex})
			startIdx = append(startIdx, i)
		}
		if len(groups) == 0 {
			// Defensive only: the window always starts at a user_msg in practice.
			continue
		}
		groups[len(groups)-1].tokens += eventEstimatedTokens(e)
	}
	return groups, startIdx
}

// estimateTokens is a rough token-count heuristic (~4 characters per token for English
// text — OpenAI's own commonly cited rule of thumb) used only to decide how many of the
// oldest turns to evict once compaction is triggered. It never drives the trigger itself
// (that's the provider-reported InputTokens in MaybeCompact) and isn't used for billing,
// so a real tokenizer dependency isn't worth adding here.
func estimateTokens(s string) int64 {
	if s == "" {
		return 0
	}
	return int64((len(s) + 3) / 4)
}

// imageTokenEstimate is a flat per-image stand-in: an image URL's length says nothing
// about the image's actual token cost, so pick a representative constant instead of
// measuring the wrong thing precisely.
const imageTokenEstimate = int64(300)

func eventEstimatedTokens(e models.TraceEvent) int64 {
	switch e.EventType {
	case models.EventTypeUserMsg:
		var p models.UserMsgPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return 0
		}
		total := estimateTokens(p.Content)
		for _, part := range p.MultiContent {
			switch part.Type {
			case llm.ContentPartText:
				total += estimateTokens(part.Text)
			case llm.ContentPartImageURL:
				total += imageTokenEstimate
			}
		}
		return total
	case models.EventTypeModelMsg:
		var p models.ModelMsgPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return 0
		}
		total := estimateTokens(p.Content)
		for _, tc := range p.ToolCalls {
			total += estimateTokens(tc.Name) + estimateTokens(tc.Arguments)
		}
		return total
	case models.EventTypeToolResult:
		var p models.ToolResultPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return 0
		}
		return estimateTokens(p.Result)
	default:
		return 0
	}
}
