package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type MemoryConfig struct {
	FactConfidenceMin   float64
	PrefConfidenceMin   float64
	SemanticDedupCosine float64
	FactsTopK           int
	EpisodesTopK        int
	EpisodeMinTurns     int
	RecentTraceEvents   int
}

type MemoryManager struct {
	trace      *repositories.TraceRepo
	prefs      *repositories.PreferenceRepo
	facts      *repositories.FactRepo
	episodes   *repositories.EpisodeRepo
	embedder   *Embedder
	extractor  *Extractor
	summarizer *Summarizer
	cfg        MemoryConfig
}

func NewMemoryManager(
	trace *repositories.TraceRepo,
	prefs *repositories.PreferenceRepo,
	facts *repositories.FactRepo,
	episodes *repositories.EpisodeRepo,
	embedder *Embedder,
	extractor *Extractor,
	summarizer *Summarizer,
	cfg MemoryConfig,
) *MemoryManager {
	return &MemoryManager{
		trace:      trace,
		prefs:      prefs,
		facts:      facts,
		episodes:   episodes,
		embedder:   embedder,
		extractor:  extractor,
		summarizer: summarizer,
		cfg:        cfg,
	}
}

type TurnContext struct {
	UserID      int64
	DialogID    int64
	UserTraceID int64
}

type RetrievedMemory struct {
	Preferences []models.Preference
	Facts       []models.Fact
	Episodes    []models.Episode
	RecentTrace []models.TraceEvent
}

// BeginTurn writes the user_msg trace event and returns a TurnContext that subsequent
// calls thread through. The returned UserTraceID is the source_trace_id for any
// candidates promoted from this turn.
func (m *MemoryManager) BeginTurn(
	userID, dialogID int64,
	msg llm.Message,
	tgMsgID int64,
) (TurnContext, error) {
	payload := models.UserMsgPayload{
		Content:      msg.Content,
		MultiContent: msg.Parts,
	}
	var tgPtr *int64
	if tgMsgID != 0 {
		tgPtr = &tgMsgID
	}
	id, err := m.trace.Append(repositories.AppendEventInput{
		UserID:      userID,
		DialogID:    dialogID,
		EventType:   models.EventTypeUserMsg,
		Payload:     payload,
		TgMessageID: tgPtr,
	})
	if err != nil {
		return TurnContext{}, fmt.Errorf("append user_msg: %w", err)
	}
	return TurnContext{UserID: userID, DialogID: dialogID, UserTraceID: id}, nil
}

// PopForRetry deletes the most recent user_msg event and everything after it in the
// current dialog, returning the popped user message so it can be replayed.
func (m *MemoryManager) PopForRetry(userID, dialogID int64) (llm.Message, int64, error) {
	e, err := m.trace.PopLatestExchange(userID, dialogID)
	if err != nil {
		return llm.Message{}, 0, err
	}
	var p models.UserMsgPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return llm.Message{}, 0, fmt.Errorf("parse user_msg payload: %w", err)
	}
	var tgMsgID int64
	if e.TgMessageID != nil {
		tgMsgID = *e.TgMessageID
	}
	msg := llm.Message{Role: llm.RoleUser}
	if len(p.MultiContent) > 0 {
		msg.Parts = p.MultiContent
	} else {
		msg.Content = p.Content
	}
	return msg, tgMsgID, nil
}

// AppendModelMsg records the assistant's response (with any tool calls) as a model_msg event.
func (m *MemoryManager) AppendModelMsg(
	mctx TurnContext,
	content string,
	toolCalls []llm.ToolCall,
	model string,
	tgMsgID int64,
) (int64, error) {
	payload := models.ModelMsgPayload{
		Content:   content,
		ToolCalls: toolCalls,
	}
	var tgPtr *int64
	if tgMsgID != 0 {
		tgPtr = &tgMsgID
	}
	return m.trace.Append(repositories.AppendEventInput{
		UserID:      mctx.UserID,
		DialogID:    mctx.DialogID,
		EventType:   models.EventTypeModelMsg,
		Payload:     payload,
		TgMessageID: tgPtr,
		Model:       model,
	})
}

// AppendToolResult records a tool's response.
func (m *MemoryManager) AppendToolResult(
	mctx TurnContext,
	toolCallID, name, result string,
) (int64, error) {
	payload := models.ToolResultPayload{
		ToolCallID: toolCallID,
		Name:       name,
		Result:     result,
	}
	return m.trace.Append(repositories.AppendEventInput{
		UserID:    mctx.UserID,
		DialogID:  mctx.DialogID,
		EventType: models.EventTypeToolResult,
		Payload:   payload,
	})
}

// RecordReminderFire records a fired reminder as a synthetic user/model exchange in
// the trace so subsequent conversation has context that the reminder went off.
func (m *MemoryManager) RecordReminderFire(
	userID, dialogID int64,
	userText, assistantText string,
	tgAssistantMsgID int64,
) error {
	_, err := m.trace.Append(repositories.AppendEventInput{
		UserID:    userID,
		DialogID:  dialogID,
		EventType: models.EventTypeUserMsg,
		Payload:   models.UserMsgPayload{Content: userText},
	})
	if err != nil {
		return err
	}
	var tgPtr *int64
	if tgAssistantMsgID != 0 {
		tgPtr = &tgAssistantMsgID
	}
	_, err = m.trace.Append(repositories.AppendEventInput{
		UserID:      userID,
		DialogID:    dialogID,
		EventType:   models.EventTypeModelMsg,
		Payload:     models.ModelMsgPayload{Content: assistantText},
		TgMessageID: tgPtr,
	})
	return err
}

// Retrieve performs scoped retrieval for the given query: all preferences,
// top-K facts (hybrid FTS5 + cosine via RRF), and the last N trace events.
func (m *MemoryManager) Retrieve(ctx context.Context, mctx TurnContext, query string) (RetrievedMemory, error) {
	var out RetrievedMemory

	prefs, err := m.prefs.GetAll(mctx.UserID)
	if err != nil {
		return out, fmt.Errorf("get preferences: %w", err)
	}
	out.Preferences = prefs

	recent, err := m.trace.GetRecent(mctx.UserID, mctx.DialogID, m.cfg.RecentTraceEvents)
	if err != nil {
		return out, fmt.Errorf("get recent trace: %w", err)
	}
	out.RecentTrace = recent

	if facts, err := m.retrieveFacts(ctx, mctx.UserID, query); err != nil {
		slog.WarnContext(ctx, "fact retrieval failed; continuing without facts", "error", err)
	} else {
		out.Facts = facts
	}

	if eps, err := m.retrieveEpisodes(ctx, mctx.UserID, query); err != nil {
		slog.WarnContext(ctx, "episode retrieval failed; continuing without episodes", "error", err)
	} else {
		out.Episodes = eps
	}

	return out, nil
}

func (m *MemoryManager) retrieveEpisodes(ctx context.Context, userID int64, query string) ([]models.Episode, error) {
	q := strings.TrimSpace(query)
	if len(q) < 3 || m.cfg.EpisodesTopK == 0 {
		return nil, nil
	}

	const candidateLimit = 20

	lexicalIDs, err := m.episodes.SearchLexical(userID, q, candidateLimit)
	if err != nil {
		return nil, fmt.Errorf("lexical episode search: %w", err)
	}

	all, err := m.episodes.ListAll(userID)
	if err != nil {
		return nil, fmt.Errorf("list episodes: %w", err)
	}

	var vectorIDs []int64
	if len(all) > 0 {
		queryEmb, err := m.embedder.Embed(ctx, q)
		if err != nil {
			slog.WarnContext(ctx, "query embedding failed; falling back to lexical only", "error", err)
		} else {
			type scored struct {
				id    int64
				score float32
			}
			scoredAll := make([]scored, 0, len(all))
			for _, e := range all {
				if e.EmbeddingModel != m.embedder.Model() {
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

	fusedIDs := rrfFuse(lexicalIDs, vectorIDs, m.cfg.EpisodesTopK)
	if len(fusedIDs) == 0 {
		return nil, nil
	}
	eps, err := m.episodes.GetByIDs(fusedIDs)
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

func (m *MemoryManager) retrieveFacts(ctx context.Context, userID int64, query string) ([]models.Fact, error) {
	q := strings.TrimSpace(query)
	if len(q) < 3 {
		return nil, nil
	}

	const candidateLimit = 20

	lexicalIDs, err := m.facts.SearchLexical(userID, q, candidateLimit)
	if err != nil {
		return nil, fmt.Errorf("lexical search: %w", err)
	}

	active, err := m.facts.ListActive(userID)
	if err != nil {
		return nil, fmt.Errorf("list active facts: %w", err)
	}

	var vectorIDs []int64
	if len(active) > 0 {
		queryEmb, err := m.embedder.Embed(ctx, q)
		if err != nil {
			slog.WarnContext(ctx, "query embedding failed; falling back to lexical only", "error", err)
		} else {
			type scored struct {
				id    int64
				score float32
			}
			scoredAll := make([]scored, 0, len(active))
			for _, f := range active {
				if f.EmbeddingModel != m.embedder.Model() {
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

	fusedIDs := rrfFuse(lexicalIDs, vectorIDs, m.cfg.FactsTopK)
	if len(fusedIDs) == 0 {
		return nil, nil
	}
	facts, err := m.facts.GetByIDs(fusedIDs)
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

// AssemblePrompt builds the LLM message list. systemHeader is the caller's base system
// prompt (constants + dynamic bits like the current date). MemoryManager appends a
// memory block to the system message and converts recent trace events into chat messages.
func (m *MemoryManager) AssemblePrompt(systemHeader string, retrieved RetrievedMemory) []llm.Message {
	var sys strings.Builder
	sys.WriteString(systemHeader)

	if len(retrieved.Preferences) > 0 {
		sys.WriteString("\n\n## User preferences\n")
		for _, p := range retrieved.Preferences {
			fmt.Fprintf(&sys, "- %s: %s\n", p.PrefKey, p.PrefValue)
		}
	}
	if len(retrieved.Facts) > 0 {
		sys.WriteString("\n## Relevant facts\n")
		for _, f := range retrieved.Facts {
			fmt.Fprintf(&sys, "- [%s] %s\n", f.Subject, f.Content)
		}
	}
	if len(retrieved.Episodes) > 0 {
		sys.WriteString("\n## Relevant past episodes\n")
		for _, e := range retrieved.Episodes {
			date := time.Unix(e.EndedAt, 0).Format("2006-01-02")
			fmt.Fprintf(&sys, "- %s: %s\n", date, e.Summary)
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

// EndTurn runs extraction + promotion gate. Designed to be called in a goroutine after
// the response has been streamed; failures are logged but never returned.
func (m *MemoryManager) EndTurn(ctx context.Context, mctx TurnContext, userMsg, assistantMsg string) {
	if userMsg == "" && assistantMsg == "" {
		return
	}
	candidates, err := m.extractor.Extract(ctx, ExtractInput{
		UserMessage:      userMsg,
		AssistantMessage: assistantMsg,
	})
	if err != nil {
		slog.WarnContext(ctx, "extractor failed", "error", err)
		return
	}
	for _, c := range candidates {
		if err := m.promote(ctx, mctx, c); err != nil {
			slog.WarnContext(ctx, "promotion failed", "error", err, "type", c.Type)
		}
	}
}

// PromoteExplicit runs the promotion gate for a caller-provided candidate (e.g. the
// LLM explicitly invoking save_memory or save_fact). The gate still enforces dedup,
// but confidence defaults to 1.0 when unset.
func (m *MemoryManager) PromoteExplicit(ctx context.Context, mctx TurnContext, c Candidate) error {
	if c.Confidence == 0 {
		c.Confidence = 1.0
	}
	return m.promote(ctx, mctx, c)
}

// RevokeFactsBySubject marks all active facts for (user, subject) as revoked.
func (m *MemoryManager) RevokeFactsBySubject(userID int64, subject string) (int64, error) {
	return m.facts.MarkRevokedBySubject(userID, subject)
}

// ListEpisodes returns all stored episodes for a user (used by the list_episodes tool).
func (m *MemoryManager) ListEpisodes(userID int64) ([]models.Episode, error) {
	return m.episodes.ListAll(userID)
}

// DeleteEpisode removes one episode after verifying it belongs to the user.
func (m *MemoryManager) DeleteEpisode(userID, id int64) error {
	return m.episodes.Delete(id, userID)
}

// CloseDialog summarizes the given (user, dialog) and writes one episodic_memory row.
// Idempotent: skips if an episode already exists for that dialog or if fewer than
// EpisodeMinTurns turns are present. Failures are logged but never returned — the
// caller (e.g. /new_chat handler) must not block on this.
func (m *MemoryManager) CloseDialog(ctx context.Context, userID, dialogID int64) {
	events, err := m.trace.GetAllForDialog(userID, dialogID)
	if err != nil {
		slog.WarnContext(ctx, "CloseDialog: read trace failed", "error", err, "user_id", userID, "dialog_id", dialogID)
		return
	}
	var turnCount int64
	var startedAt, endedAt int64
	for _, e := range events {
		if e.EventType == models.EventTypeUserMsg || e.EventType == models.EventTypeModelMsg {
			turnCount++
		}
		if startedAt == 0 || e.CreatedAt < startedAt {
			startedAt = e.CreatedAt
		}
		if e.CreatedAt > endedAt {
			endedAt = e.CreatedAt
		}
	}
	if turnCount < int64(m.cfg.EpisodeMinTurns) {
		return
	}

	exists, err := m.episodes.ExistsForDialog(userID, dialogID)
	if err != nil {
		slog.WarnContext(ctx, "CloseDialog: existence check failed", "error", err)
		return
	}
	if exists {
		return
	}

	summary, err := m.summarizer.Summarize(ctx, events)
	if err != nil {
		slog.WarnContext(ctx, "CloseDialog: summarizer failed", "error", err)
		return
	}
	if strings.TrimSpace(summary) == "" {
		return
	}

	emb, err := m.embedder.Embed(ctx, summary)
	if err != nil {
		slog.WarnContext(ctx, "CloseDialog: embed failed", "error", err)
		return
	}

	if _, err := m.episodes.Insert(repositories.InsertEpisodeInput{
		UserID:         userID,
		DialogID:       dialogID,
		Summary:        summary,
		StartedAt:      startedAt,
		EndedAt:        endedAt,
		TurnCount:      turnCount,
		Embedding:      emb,
		EmbeddingModel: m.embedder.Model(),
	}); err != nil {
		slog.WarnContext(ctx, "CloseDialog: insert failed", "error", err)
		return
	}
	slog.InfoContext(ctx, "dialog summarized", "user_id", userID, "dialog_id", dialogID, "turn_count", turnCount)
}

func (m *MemoryManager) promote(ctx context.Context, mctx TurnContext, c Candidate) error {
	switch c.Type {
	case CandidatePreference:
		if c.Confidence < m.cfg.PrefConfidenceMin || c.Key == "" || c.Value == "" {
			return nil
		}
		if _, err := ValidatePreference(c.Key, c.Value); err != nil {
			slog.WarnContext(ctx, "Dropping invalid inferred preference",
				"key", c.Key, "value", c.Value, "error", err)
			return nil
		}
		traceID := mctx.UserTraceID
		return m.prefs.Upsert(repositories.UpsertPreferenceInput{
			UserID:        mctx.UserID,
			Key:           c.Key,
			Value:         c.Value,
			Source:        models.PreferenceSourceInferred,
			SourceTraceID: &traceID,
		})

	case CandidateFact:
		if c.Confidence < m.cfg.FactConfidenceMin || c.Subject == "" || c.Content == "" {
			return nil
		}
		hash := contentHash(c.Content)
		if existing, err := m.facts.GetByContentHash(mctx.UserID, hash); err != nil {
			return fmt.Errorf("hash lookup: %w", err)
		} else if existing != nil {
			return nil
		}

		emb, err := m.embedder.Embed(ctx, c.Subject+" "+c.Content)
		if err != nil {
			return fmt.Errorf("embed fact: %w", err)
		}

		sameSubject, err := m.facts.ListActiveBySubject(mctx.UserID, c.Subject)
		if err != nil {
			return fmt.Errorf("list same subject: %w", err)
		}
		for _, ex := range sameSubject {
			if ex.EmbeddingModel != m.embedder.Model() {
				continue
			}
			if vec.Cosine(emb, ex.Embedding) >= float32(m.cfg.SemanticDedupCosine) {
				return nil
			}
		}

		_, err = m.facts.Insert(repositories.InsertFactInput{
			UserID:         mctx.UserID,
			Subject:        c.Subject,
			Content:        c.Content,
			ContentHash:    hash,
			Confidence:     c.Confidence,
			Status:         models.FactStatusActive,
			SourceTraceID:  mctx.UserTraceID,
			Embedding:      emb,
			EmbeddingModel: m.embedder.Model(),
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
