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

	// Facts and episodes are both scored against the same query embedding; compute it
	// once here rather than once per kind, since it's a real network round-trip.
	queryEmb, hasQueryEmb := r.embedQuery(ctx, query)

	if facts, err := r.retrieveFacts(ctx, mctx.UserID, query, queryEmb, hasQueryEmb); err != nil {
		slog.WarnContext(ctx, "fact retrieval failed; continuing without facts", "error", err)
	} else {
		out.Facts = facts
	}

	if eps, err := r.retrieveEpisodes(ctx, mctx.UserID, query, queryEmb, hasQueryEmb); err != nil {
		slog.WarnContext(ctx, "episode retrieval failed; continuing without episodes", "error", err)
	} else {
		out.Episodes = eps
	}

	return out, nil
}

// embedQuery computes the embedding shared by fact and episode retrieval. ok is false
// for queries too short to be meaningful or when embedding fails; callers fall back to
// lexical-only scoring in either case.
func (r *Retriever) embedQuery(ctx context.Context, query string) (emb []float32, ok bool) {
	q := strings.TrimSpace(query)
	if len(q) < 3 {
		return nil, false
	}
	emb, err := r.embedder.Embed(ctx, q)
	if err != nil {
		slog.WarnContext(ctx, "query embedding failed; falling back to lexical only", "error", err)
		return nil, false
	}
	return emb, true
}

func (r *Retriever) retrieveEpisodes(ctx context.Context, userID int64, query string, queryEmb []float32, hasQueryEmb bool) ([]models.Episode, error) {
	if r.cfg.EpisodesTopK == 0 {
		return nil, nil
	}
	return hybridSearch(
		userID, query, queryEmb, hasQueryEmb, r.embedder.Model(), r.cfg.EpisodesTopK,
		r.episodes.SearchLexical,
		r.episodes.ListAll,
		func(e models.Episode) (int64, string, []float32) { return e.ID, e.EmbeddingModel, e.Embedding },
		r.episodes.GetByIDs,
	)
}

func (r *Retriever) retrieveFacts(ctx context.Context, userID int64, query string, queryEmb []float32, hasQueryEmb bool) ([]models.Fact, error) {
	return hybridSearch(
		userID, query, queryEmb, hasQueryEmb, r.embedder.Model(), r.cfg.FactsTopK,
		r.facts.SearchLexical,
		r.facts.ListActive,
		func(f models.Fact) (int64, string, []float32) { return f.ID, f.EmbeddingModel, f.Embedding },
		r.facts.GetByIDs,
	)
}

// hybridSearch is the retrieval algorithm shared by every memory kind: rank candidates
// lexically (FTS5) and by cosine similarity against the query embedding, fuse the two
// rankings (RRF), then load and order the fused items. Each kind supplies only how to
// fetch its own candidates and how to read an id/embedding off one.
func hybridSearch[T any](
	userID int64,
	query string,
	queryEmb []float32,
	hasQueryEmb bool,
	embedderModel string,
	topN int,
	searchLexical func(userID int64, query string, limit int) ([]int64, error),
	listCandidates func(userID int64) ([]T, error),
	embeddingOf func(T) (id int64, embeddingModel string, embedding []float32),
	getByIDs func(ids []int64) ([]T, error),
) ([]T, error) {
	q := strings.TrimSpace(query)
	if len(q) < 3 {
		return nil, nil
	}

	const candidateLimit = 20

	lexicalIDs, err := searchLexical(userID, q, candidateLimit)
	if err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}

	var vectorIDs []int64
	if hasQueryEmb {
		candidates, err := listCandidates(userID)
		if err != nil {
			return nil, fmt.Errorf("list candidates: %w", err)
		}
		type scored struct {
			id    int64
			score float32
		}
		scoredAll := make([]scored, 0, len(candidates))
		for _, c := range candidates {
			id, model, embedding := embeddingOf(c)
			if model != embedderModel {
				continue
			}
			scoredAll = append(scoredAll, scored{id: id, score: vec.Cosine(queryEmb, embedding)})
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

	fusedIDs := rrfFuse(lexicalIDs, vectorIDs, topN)
	if len(fusedIDs) == 0 {
		return nil, nil
	}
	items, err := getByIDs(fusedIDs)
	if err != nil {
		return nil, fmt.Errorf("load fused items: %w", err)
	}
	idx := make(map[int64]T, len(items))
	for _, item := range items {
		id, _, _ := embeddingOf(item)
		idx[id] = item
	}
	ordered := make([]T, 0, len(fusedIDs))
	for _, id := range fusedIDs {
		if item, ok := idx[id]; ok {
			ordered = append(ordered, item)
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
