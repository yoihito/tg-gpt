package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/vec"
)

// RetrievalConfig bounds how much of each memory kind Retrieve pulls in for a query.
type RetrievalConfig struct {
	FactsTopK         int
	EpisodesTopK      int
	RecentTraceEvents int
}

// Retriever assembles the memory context for a turn: all preferences, top-K facts and
// episodes (hybrid FTS5 + cosine via RRF) for the current query, and the last N trace
// events — then renders that into the llm.Message history sent to the model. It owns no
// trace-journal writes or promotion/consolidation logic — see TraceStore and
// MemoryConsolidator for those.
type Retriever struct {
	trace    *repositories.TraceRepo
	prefs    *repositories.PreferenceRepo
	facts    *repositories.FactRepo
	episodes *repositories.EpisodeRepo
	embedder *Embedder
	cfg      RetrievalConfig
}

func NewRetriever(
	trace *repositories.TraceRepo,
	prefs *repositories.PreferenceRepo,
	facts *repositories.FactRepo,
	episodes *repositories.EpisodeRepo,
	embedder *Embedder,
	cfg RetrievalConfig,
) *Retriever {
	return &Retriever{
		trace:    trace,
		prefs:    prefs,
		facts:    facts,
		episodes: episodes,
		embedder: embedder,
		cfg:      cfg,
	}
}

type RetrievedMemory struct {
	Preferences []models.Preference
	Facts       []models.Fact
	Episodes    []models.Episode
	RecentTrace []models.TraceEvent
}

// Retrieve performs scoped retrieval for the given query: all preferences,
// top-K facts (hybrid FTS5 + cosine via RRF), and the last N trace events.
func (r *Retriever) Retrieve(ctx context.Context, mctx TurnContext, query string) (RetrievedMemory, error) {
	var out RetrievedMemory

	prefs, err := r.prefs.GetAll(mctx.UserID)
	if err != nil {
		return out, fmt.Errorf("get preferences: %w", err)
	}
	out.Preferences = prefs

	recent, err := r.trace.GetRecent(mctx.UserID, mctx.DialogID, r.cfg.RecentTraceEvents)
	if err != nil {
		return out, fmt.Errorf("get recent trace: %w", err)
	}
	out.RecentTrace = recent

	if facts, err := r.retrieveFacts(ctx, mctx.UserID, query); err != nil {
		slog.WarnContext(ctx, "fact retrieval failed; continuing without facts", "error", err)
	} else {
		out.Facts = facts
	}

	if eps, err := r.retrieveEpisodes(ctx, mctx.UserID, query); err != nil {
		slog.WarnContext(ctx, "episode retrieval failed; continuing without episodes", "error", err)
	} else {
		out.Episodes = eps
	}

	return out, nil
}

func (r *Retriever) retrieveEpisodes(ctx context.Context, userID int64, query string) ([]models.Episode, error) {
	q := strings.TrimSpace(query)
	if len(q) < 3 || r.cfg.EpisodesTopK == 0 {
		return nil, nil
	}

	const candidateLimit = 20

	lexicalIDs, err := r.episodes.SearchLexical(userID, q, candidateLimit)
	if err != nil {
		return nil, fmt.Errorf("lexical episode search: %w", err)
	}

	all, err := r.episodes.ListAll(userID)
	if err != nil {
		return nil, fmt.Errorf("list episodes: %w", err)
	}

	var vectorIDs []int64
	if len(all) > 0 {
		queryEmb, err := r.embedder.Embed(ctx, q)
		if err != nil {
			slog.WarnContext(ctx, "query embedding failed; falling back to lexical only", "error", err)
		} else {
			type scored struct {
				id    int64
				score float32
			}
			scoredAll := make([]scored, 0, len(all))
			for _, e := range all {
				if e.EmbeddingModel != r.embedder.Model() {
					continue
				}
				scoredAll = append(scoredAll, scored{id: e.ID, score: vec.Cosine(queryEmb, e.Embedding)})
			}
			sort.Slice(scoredAll, func(i, j int) bool { return scoredAll[i].score > scoredAll[j].score })
			limit := candidateLimit
			if limit > len(scoredAll) {
				limit = len(scoredAll)
			}
			vectorIDs = make([]int64, 0, limit)
			for i := 0; i < limit; i++ {
				vectorIDs = append(vectorIDs, scoredAll[i].id)
			}
		}
	}

	fusedIDs := rrfFuse(lexicalIDs, vectorIDs, r.cfg.EpisodesTopK)
	if len(fusedIDs) == 0 {
		return nil, nil
	}
	eps, err := r.episodes.GetByIDs(fusedIDs)
	if err != nil {
		return nil, fmt.Errorf("load fused episodes: %w", err)
	}
	idx := make(map[int64]models.Episode, len(eps))
	for _, e := range eps {
		idx[e.ID] = e
	}
	ordered := make([]models.Episode, 0, len(fusedIDs))
	for _, id := range fusedIDs {
		if e, ok := idx[id]; ok {
			ordered = append(ordered, e)
		}
	}
	return ordered, nil
}

func (r *Retriever) retrieveFacts(ctx context.Context, userID int64, query string) ([]models.Fact, error) {
	q := strings.TrimSpace(query)
	if len(q) < 3 {
		return nil, nil
	}

	const candidateLimit = 20

	lexicalIDs, err := r.facts.SearchLexical(userID, q, candidateLimit)
	if err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}

	active, err := r.facts.ListActive(userID)
	if err != nil {
		return nil, fmt.Errorf("list active facts: %w", err)
	}

	var vectorIDs []int64
	if len(active) > 0 {
		queryEmb, err := r.embedder.Embed(ctx, q)
		if err != nil {
			slog.WarnContext(ctx, "query embedding failed; falling back to lexical only", "error", err)
		} else {
			type scored struct {
				id    int64
				score float32
			}
			scoredAll := make([]scored, 0, len(active))
			for _, f := range active {
				if f.EmbeddingModel != r.embedder.Model() {
					continue
				}
				scoredAll = append(scoredAll, scored{id: f.ID, score: vec.Cosine(queryEmb, f.Embedding)})
			}
			sort.Slice(scoredAll, func(i, j int) bool { return scoredAll[i].score > scoredAll[j].score })
			limit := candidateLimit
			if limit > len(scoredAll) {
				limit = len(scoredAll)
			}
			vectorIDs = make([]int64, 0, limit)
			for i := 0; i < limit; i++ {
				vectorIDs = append(vectorIDs, scoredAll[i].id)
			}
		}
	}

	fusedIDs := rrfFuse(lexicalIDs, vectorIDs, r.cfg.FactsTopK)
	if len(fusedIDs) == 0 {
		return nil, nil
	}
	facts, err := r.facts.GetByIDs(fusedIDs)
	if err != nil {
		return nil, fmt.Errorf("load fused facts: %w", err)
	}
	idx := make(map[int64]models.Fact, len(facts))
	for _, f := range facts {
		idx[f.ID] = f
	}
	ordered := make([]models.Fact, 0, len(fusedIDs))
	for _, id := range fusedIDs {
		if f, ok := idx[id]; ok {
			ordered = append(ordered, f)
		}
	}
	return ordered, nil
}

// rrfFuse merges two ranked id lists with reciprocal rank fusion (k=60) and returns
// the top-N ids by fused score.
func rrfFuse(a, b []int64, topN int) []int64 {
	const k = 60
	scores := make(map[int64]float64)
	for rank, id := range a {
		scores[id] += 1.0 / float64(k+rank+1)
	}
	for rank, id := range b {
		scores[id] += 1.0 / float64(k+rank+1)
	}
	type rs struct {
		id    int64
		score float64
	}
	ranked := make([]rs, 0, len(scores))
	for id, s := range scores {
		ranked = append(ranked, rs{id, s})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if topN > len(ranked) {
		topN = len(ranked)
	}
	out := make([]int64, 0, topN)
	for i := 0; i < topN; i++ {
		out = append(out, ranked[i].id)
	}
	return out
}

// AssemblePrompt builds the stable part of the LLM message list: systemHeader (the
// caller's base system prompt — constants + dynamic bits like the current date) plus
// preferences, which are per-user but not per-query, so they stay byte-identical across
// turns whose retrieval differs but whose actual conversation doesn't. Query-dependent
// retrieval (facts, episodes) is deliberately NOT included here — see
// AppendRetrievalContext — so this prefix (and the trace replay after it) stays eligible
// for the provider's prompt cache instead of being invalidated by every new query.
func (r *Retriever) AssemblePrompt(systemHeader string, retrieved RetrievedMemory) []llm.Message {
	var sys strings.Builder
	sys.WriteString(systemHeader)

	if len(retrieved.Preferences) > 0 {
		sys.WriteString("\n\n## User preferences\n")
		for _, p := range retrieved.Preferences {
			fmt.Fprintf(&sys, "- %s: %s\n", p.PrefKey, p.PrefValue)
		}
	}

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: sys.String()},
	}
	// Trim leading trace events until we hit a user_msg so we never start the replay
	// mid-tool-call-group (which would leave an orphan `role: tool` message that OpenAI rejects).
	start := 0
	for ; start < len(retrieved.RecentTrace); start++ {
		if retrieved.RecentTrace[start].EventType == models.EventTypeUserMsg {
			break
		}
	}
	messages = appendTraceMessages(messages, retrieved.RecentTrace[start:])
	return messages
}

// AppendRetrievalContext appends facts/episodes as a trailing message, if there are any.
// These come from a per-query semantic search, so they differ on nearly every turn.
// Placing them after everything else — including the current turn's own input, appended
// separately via appendMissingCurrentInputs — keeps that volatility from invalidating a
// cached prefix of the (usually much larger, and often unchanged) system+trace history
// before them.
func (r *Retriever) AppendRetrievalContext(history []llm.Message, retrieved RetrievedMemory) []llm.Message {
	var sys strings.Builder
	if len(retrieved.Facts) > 0 {
		sys.WriteString("## Relevant facts\n")
		for _, f := range retrieved.Facts {
			fmt.Fprintf(&sys, "- [%s] %s\n", f.Subject, f.Content)
		}
	}
	if len(retrieved.Episodes) > 0 {
		if sys.Len() > 0 {
			sys.WriteString("\n")
		}
		sys.WriteString("## Relevant past episodes\n")
		for _, e := range retrieved.Episodes {
			date := time.Unix(e.EndedAt, 0).Format("2006-01-02")
			fmt.Fprintf(&sys, "- %s: %s\n", date, e.Summary)
		}
	}
	if sys.Len() == 0 {
		return history
	}
	return append(history, llm.Message{Role: llm.RoleSystem, Content: sys.String()})
}

func appendTraceMessages(messages []llm.Message, events []models.TraceEvent) []llm.Message {
	for i := 0; i < len(events); {
		msg, ok := traceEventToMessage(events[i])
		if !ok {
			i++
			continue
		}

		if msg.Role == llm.RoleTool {
			i++
			continue
		}

		if msg.Role != llm.RoleAssistant || len(msg.ToolCalls) == 0 {
			messages = append(messages, msg)
			i++
			continue
		}

		required := make(map[string]struct{}, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			required[call.ID] = struct{}{}
		}

		group := []llm.Message{msg}
		j := i + 1
		for ; j < len(events); j++ {
			next, ok := traceEventToMessage(events[j])
			if !ok {
				continue
			}
			if next.Role != llm.RoleTool {
				break
			}
			group = append(group, next)
			if next.ToolResult != nil {
				delete(required, next.ToolResult.CallID)
			}
			if len(required) == 0 {
				j++
				break
			}
		}

		if len(required) == 0 {
			messages = append(messages, group...)
		} else {
			slog.Warn("Skipping incomplete tool-call trace group", "missing_tool_results", len(required))
		}
		i = j
	}
	return messages
}

func traceEventToMessage(e models.TraceEvent) (llm.Message, bool) {
	switch e.EventType {
	case models.EventTypeUserMsg:
		var p models.UserMsgPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return llm.Message{}, false
		}
		msg := llm.Message{Role: llm.RoleUser}
		if len(p.MultiContent) > 0 {
			msg.Parts = p.MultiContent
		} else {
			msg.Content = p.Content
		}
		return msg, true
	case models.EventTypeModelMsg:
		var p models.ModelMsgPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return llm.Message{}, false
		}
		return llm.Message{
			Role:      llm.RoleAssistant,
			Content:   p.Content,
			ToolCalls: p.ToolCalls,
		}, true
	case models.EventTypeToolResult:
		var p models.ToolResultPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return llm.Message{}, false
		}
		return llm.Message{
			Role: llm.RoleTool,
			ToolResult: &llm.ToolResult{
				CallID: p.ToolCallID,
				Name:   p.Name,
				Output: p.Result,
			},
		}, true
	}
	return llm.Message{}, false
}
