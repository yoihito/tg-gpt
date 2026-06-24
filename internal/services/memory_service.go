package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

type MemoryService struct {
	prefs         *repositories.PreferenceRepo
	memoryManager *MemoryManager
}

func NewMemoryService(prefs *repositories.PreferenceRepo, memoryManager *MemoryManager) *MemoryService {
	return &MemoryService{prefs: prefs, memoryManager: memoryManager}
}

func (s *MemoryService) GetMemoryTools() []openai.Tool {
	return []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "save_memory",
				Description: "Save a stable user preference (e.g. timezone, language, format, communication style). For durable facts about the user, use save_fact instead.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{
							"type":        "string",
							"description": "Short snake_case key (e.g. 'timezone', 'response_language', 'tone').",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The preference value.",
						},
					},
					"required": []string{"key", "content"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "get_memory",
				Description: "Retrieve a specific preference by key.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{"type": "string"},
					},
					"required": []string{"key"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "list_memories",
				Description: "List all stored preferences for the user.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "delete_memory",
				Description: "Delete a preference by key.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{"type": "string"},
					},
					"required": []string{"key"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "save_fact",
				Description: "Save a durable fact about the user, their life, work, or relationships. Use this for things you want to remember across sessions. For preferences (format, language, etc.) use save_memory instead.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"subject": map[string]interface{}{
							"type":        "string",
							"description": "Short snake_case subject (e.g. 'self', 'wife_anna', 'company_acme').",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "One-sentence statement of the fact.",
						},
					},
					"required": []string{"subject", "content"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "forget_about",
				Description: "Mark all facts about a given subject as revoked (e.g. the user wants you to forget their job).",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"subject": map[string]interface{}{"type": "string"},
					},
					"required": []string{"subject"},
				},
			},
		},
	}
}

func (s *MemoryService) HandleToolCall(ctx context.Context, mctx TurnContext, toolCall openai.ToolCall) (string, error) {
	if toolCall.Function.Name == "list_memories" {
		return s.handleListMemories(mctx.UserID)
	}

	if strings.TrimSpace(toolCall.Function.Arguments) == "" {
		return "", fmt.Errorf("empty arguments for tool call: %s", toolCall.Function.Name)
	}

	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &probe); err != nil {
		return "", fmt.Errorf("invalid JSON arguments for %s: %w - arguments: %s", toolCall.Function.Name, err, toolCall.Function.Arguments)
	}

	switch toolCall.Function.Name {
	case "save_memory":
		return s.handleSaveMemory(mctx, toolCall.Function.Arguments)
	case "get_memory":
		return s.handleGetMemory(mctx.UserID, toolCall.Function.Arguments)
	case "delete_memory":
		return s.handleDeleteMemory(mctx.UserID, toolCall.Function.Arguments)
	case "save_fact":
		return s.handleSaveFact(ctx, mctx, toolCall.Function.Arguments)
	case "forget_about":
		return s.handleForgetAbout(mctx.UserID, toolCall.Function.Arguments)
	default:
		return "", fmt.Errorf("unknown tool call: %s", toolCall.Function.Name)
	}
}

func (s *MemoryService) handleSaveMemory(mctx TurnContext, arguments string) (string, error) {
	var args struct {
		Key     string `json:"key"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for save_memory: %w", err)
	}
	traceID := mctx.UserTraceID
	err := s.prefs.Upsert(repositories.UpsertPreferenceInput{
		UserID:        mctx.UserID,
		Key:           args.Key,
		Value:         args.Content,
		Source:        models.PreferenceSourceExplicit,
		SourceTraceID: &traceID,
	})
	if err != nil {
		slog.Error("Failed to save preference", "error", err, "user_id", mctx.UserID)
		return "Failed to save preference", err
	}
	return fmt.Sprintf("Preference saved: %s = %s", args.Key, args.Content), nil
}

func (s *MemoryService) handleGetMemory(userID int64, arguments string) (string, error) {
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for get_memory: %w", err)
	}
	p, err := s.prefs.Get(userID, args.Key)
	if err != nil {
		return "", err
	}
	if p == nil {
		return fmt.Sprintf("No preference for key: %s", args.Key), nil
	}
	return fmt.Sprintf("Preference '%s': %s", p.PrefKey, p.PrefValue), nil
}

func (s *MemoryService) handleListMemories(userID int64) (string, error) {
	prefs, err := s.prefs.GetAll(userID)
	if err != nil {
		return "", err
	}
	if len(prefs) == 0 {
		return "No preferences stored.", nil
	}
	var b strings.Builder
	b.WriteString("Preferences:\n")
	for _, p := range prefs {
		fmt.Fprintf(&b, "- %s: %s\n", p.PrefKey, p.PrefValue)
	}
	return b.String(), nil
}

func (s *MemoryService) handleDeleteMemory(userID int64, arguments string) (string, error) {
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for delete_memory: %w", err)
	}
	if err := s.prefs.Delete(userID, args.Key); err != nil {
		return "", err
	}
	return fmt.Sprintf("Preference deleted: %s", args.Key), nil
}

func (s *MemoryService) handleSaveFact(ctx context.Context, mctx TurnContext, arguments string) (string, error) {
	var args struct {
		Subject string `json:"subject"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for save_fact: %w", err)
	}
	if args.Subject == "" || args.Content == "" {
		return "subject and content are required", nil
	}
	err := s.memoryManager.PromoteExplicit(ctx, mctx, Candidate{
		Type:       CandidateFact,
		Subject:    args.Subject,
		Content:    args.Content,
		Confidence: 1.0,
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to save fact", "error", err, "user_id", mctx.UserID)
		return "Failed to save fact", err
	}
	return fmt.Sprintf("Fact saved about %s: %s", args.Subject, args.Content), nil
}

func (s *MemoryService) handleForgetAbout(userID int64, arguments string) (string, error) {
	var args struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for forget_about: %w", err)
	}
	if args.Subject == "" {
		return "subject is required", nil
	}
	n, err := s.memoryManager.RevokeFactsBySubject(userID, args.Subject)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Revoked %d fact(s) about %s.", n, args.Subject), nil
}
