package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

func TestTextServiceIntegrationSimpleTextTurnPersistsTraceAndUsage(t *testing.T) {
	h := newTextServiceIntegrationHarness(t, [][]llm.StreamEvent{
		{
			{Usage: &llm.Usage{InputTokens: 3, OutputTokens: 4}},
			{TextDelta: "Hello"},
			{TextDelta: ", Vadim."},
		},
	})

	_, err := h.textService.handleLLMRequest(context.Background(), h.user, 101, llm.Message{
		Role:    llm.RoleUser,
		Content: "hello",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	events := h.traceEvents(t, h.user.CurrentDialogId)
	if len(events) != 2 {
		t.Fatalf("trace events len: got %d want 2: %#v", len(events), events)
	}
	if events[0].EventType != models.EventTypeUserMsg {
		t.Fatalf("first event type: got %q", events[0].EventType)
	}
	userPayload := decodePayload[models.UserMsgPayload](t, events[0].Payload)
	if userPayload.Content != "hello" {
		t.Fatalf("user content: got %q", userPayload.Content)
	}
	if events[0].TgMessageID == nil || *events[0].TgMessageID != 101 {
		t.Fatalf("user tg message id: got %#v", events[0].TgMessageID)
	}

	if events[1].EventType != models.EventTypeModelMsg {
		t.Fatalf("second event type: got %q", events[1].EventType)
	}
	modelPayload := decodePayload[models.ModelMsgPayload](t, events[1].Payload)
	if modelPayload.Content != "Hello, Vadim." {
		t.Fatalf("assistant content: got %q", modelPayload.Content)
	}

	updated, err := h.userRepo.GetUser(h.user.Id)
	if err != nil {
		t.Fatal(err)
	}
	if updated.NumberOfInputTokens != 3 || updated.NumberOfOutputTokens != 4 {
		t.Fatalf("usage: got input=%d output=%d", updated.NumberOfInputTokens, updated.NumberOfOutputTokens)
	}
}

func TestTextServiceIntegrationToolLoopPersistsProtocolOrderedTrace(t *testing.T) {
	h := newTextServiceIntegrationHarness(t, [][]llm.StreamEvent{
		{
			{ToolCalls: []llm.ToolCall{{
				ID:        "call_1",
				Index:     0,
				Name:      "list_memories",
				Arguments: "{}",
			}}},
		},
		{
			{TextDelta: "You have no saved preferences."},
		},
	})

	_, err := h.textService.handleLLMRequest(context.Background(), h.user, 201, llm.Message{
		Role:    llm.RoleUser,
		Content: "what do you remember?",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	events := h.traceEvents(t, h.user.CurrentDialogId)
	if len(events) != 4 {
		t.Fatalf("trace events len: got %d want 4: %#v", len(events), events)
	}
	wantTypes := []string{
		models.EventTypeUserMsg,
		models.EventTypeModelMsg,
		models.EventTypeToolResult,
		models.EventTypeModelMsg,
	}
	for i, want := range wantTypes {
		if events[i].EventType != want {
			t.Fatalf("event %d type: got %q want %q", i, events[i].EventType, want)
		}
	}

	toolCallPayload := decodePayload[models.ModelMsgPayload](t, events[1].Payload)
	if len(toolCallPayload.ToolCalls) != 1 || toolCallPayload.ToolCalls[0].Name != "list_memories" {
		t.Fatalf("tool call payload: %#v", toolCallPayload)
	}
	toolResultPayload := decodePayload[models.ToolResultPayload](t, events[2].Payload)
	if toolResultPayload.ToolCallID != "call_1" || toolResultPayload.Name != "list_memories" {
		t.Fatalf("tool result payload: %#v", toolResultPayload)
	}
	finalPayload := decodePayload[models.ModelMsgPayload](t, events[3].Payload)
	if finalPayload.Content != "You have no saved preferences." {
		t.Fatalf("final content: got %q", finalPayload.Content)
	}

	requests := h.llmClient.requestsSnapshot()
	if len(requests) != 2 {
		t.Fatalf("llm requests: got %d want 2", len(requests))
	}
	second := requests[1].Messages
	if len(second) < 4 {
		t.Fatalf("second request messages too short: %#v", second)
	}
	gotSuffix := second[len(second)-3:]
	if gotSuffix[0].Role != llm.RoleUser || gotSuffix[1].Role != llm.RoleAssistant || gotSuffix[2].Role != llm.RoleTool {
		t.Fatalf("second request suffix roles: %#v", gotSuffix)
	}
	if len(gotSuffix[1].ToolCalls) != 1 || gotSuffix[1].ToolCalls[0].ID != "call_1" {
		t.Fatalf("second request tool call: %#v", gotSuffix[1])
	}
	if gotSuffix[2].ToolResult == nil || gotSuffix[2].ToolResult.CallID != "call_1" {
		t.Fatalf("second request tool result: %#v", gotSuffix[2])
	}
}

func TestTextServiceIntegrationRetryReplacesLastExchange(t *testing.T) {
	h := newTextServiceIntegrationHarness(t, [][]llm.StreamEvent{
		{{TextDelta: "first answer"}},
		{{TextDelta: "retry answer"}},
	})

	_, err := h.textService.handleLLMRequest(context.Background(), h.user, 301, llm.Message{
		Role:    llm.RoleUser,
		Content: "try this",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	userMsg, tgMsgID, err := h.memoryManager.PopForRetry(h.user.Id, h.user.CurrentDialogId)
	if err != nil {
		t.Fatal(err)
	}
	if tgMsgID != 301 || userMsg.Content != "try this" {
		t.Fatalf("popped message: tg=%d msg=%#v", tgMsgID, userMsg)
	}

	if err := h.textService.RetryWithMessage(context.Background(), h.user, tgMsgID, userMsg, nil); err != nil {
		t.Fatal(err)
	}

	events := h.traceEvents(t, h.user.CurrentDialogId)
	if len(events) != 2 {
		t.Fatalf("trace events len after retry: got %d want 2: %#v", len(events), events)
	}
	finalPayload := decodePayload[models.ModelMsgPayload](t, events[1].Payload)
	if finalPayload.Content != "retry answer" {
		t.Fatalf("final retry content: got %q", finalPayload.Content)
	}
}

type textServiceIntegrationHarness struct {
	db            *database.DB
	user          models.User
	userRepo      *repositories.UserRepo
	traceRepo     *repositories.TraceRepo
	memoryManager *MemoryManager
	textService   *TextService
	llmClient     *fakeLLMClient
}

func newTextServiceIntegrationHarness(t *testing.T, streams [][]llm.StreamEvent) *textServiceIntegrationHarness {
	t.Helper()

	db, err := database.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Logf("close db: %v", err)
		}
	})
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	userRepo := repositories.NewUserRepo(db)
	traceRepo := repositories.NewTraceRepo(db)
	prefRepo := repositories.NewPreferenceRepo(db)
	factRepo := repositories.NewFactRepo(db)
	episodeRepo := repositories.NewEpisodeRepo(db)
	reminderRepo := repositories.NewReminderRepo(db)

	openaiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"{\"candidates\":[]}"}}]}`))
	}))
	t.Cleanup(openaiServer.Close)
	openaiConfig := openai.DefaultConfig("test-token")
	openaiConfig.BaseURL = openaiServer.URL + "/v1"
	openaiClient := openai.NewClientWithConfig(openaiConfig)

	memoryManager := NewMemoryManager(
		traceRepo,
		prefRepo,
		factRepo,
		episodeRepo,
		NewEmbedder(openaiClient, "test-embedding"),
		NewExtractor(openaiClient, "test-extractor"),
		NewSummarizer(openaiClient, "test-summarizer"),
		MemoryConfig{
			FactConfidenceMin:   0.8,
			PrefConfidenceMin:   0.8,
			SemanticDedupCosine: 0.95,
			FactsTopK:           3,
			EpisodesTopK:        3,
			EpisodeMinTurns:     2,
			RecentTraceEvents:   20,
		},
	)
	memoryService := NewMemoryService(prefRepo, memoryManager)
	reminderService := NewReminderService(reminderRepo, userRepo, prefRepo, memoryManager, nil)
	llmClient := &fakeLLMClient{
		models:  map[string]struct{}{"test-model": {}},
		streams: streams,
	}
	textService := NewTextService(
		llmClient,
		userRepo,
		memoryService,
		memoryManager,
		reminderService,
		nil,
		int64(time.Hour.Seconds()),
		"test-model",
	)

	user, err := userRepo.Register(12345, "Vadim", "", "vadim", 54321, true, "test-model")
	if err != nil {
		t.Fatal(err)
	}

	return &textServiceIntegrationHarness{
		db:            db,
		user:          user,
		userRepo:      userRepo,
		traceRepo:     traceRepo,
		memoryManager: memoryManager,
		textService:   textService,
		llmClient:     llmClient,
	}
}

func (h *textServiceIntegrationHarness) traceEvents(t *testing.T, dialogID int64) []models.TraceEvent {
	t.Helper()
	events, err := h.traceRepo.GetAllForDialog(h.user.Id, dialogID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func decodePayload[T any](t *testing.T, payload json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type fakeLLMClient struct {
	mu       sync.Mutex
	models   map[string]struct{}
	streams  [][]llm.StreamEvent
	requests []llm.Request
}

func (c *fakeLLMClient) IsClientRegistered(modelID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.models[modelID]
	return ok
}

func (c *fakeLLMClient) Stream(ctx context.Context, request llm.Request) (llm.Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	if len(c.streams) == 0 {
		return &fakeStream{err: io.EOF}, nil
	}
	events := c.streams[0]
	c.streams = c.streams[1:]
	return &fakeStream{events: events, err: io.EOF}, nil
}

func (c *fakeLLMClient) requestsSnapshot() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.Request, len(c.requests))
	copy(out, c.requests)
	return out
}

type fakeStream struct {
	events []llm.StreamEvent
	idx    int
	err    error
}

func (s *fakeStream) Next() bool {
	if s.idx >= len(s.events) {
		return false
	}
	s.idx++
	return true
}

func (s *fakeStream) Event() llm.StreamEvent {
	return s.events[s.idx-1]
}

func (s *fakeStream) Err() error {
	return s.err
}

func (s *fakeStream) Close() error {
	return nil
}
