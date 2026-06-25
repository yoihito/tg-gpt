package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"vadimgribanov.com/tg-gpt/internal/llm"
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

func (s *MemoryService) GetMemoryTools() []llm.Tool {
	return []llm.Tool{
		{
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
		{
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
		{
			Name:        "list_memories",
			Description: "List all stored preferences for the user.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
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
		{
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
		{
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
		{
			Name:        "list_episodes",
			Description: "List stored dialog summaries (episodic memory) for the user. Each entry has an ID, date, and short summary.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "forget_episode",
			Description: "Delete a stored episode by its ID. Use after list_episodes to find the right ID.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"episode_id": map[string]interface{}{
						"type":        "integer",
						"description": "The ID of the episode to delete (from list_episodes).",
					},
				},
				"required": []string{"episode_id"},
			},
		},
	}
}

func (s *MemoryService) HandleToolCall(ctx context.Context, mctx TurnContext, toolCall llm.ToolCall) (string, error) {
	switch toolCall.Name {
	case "list_memories":
		return s.handleListMemories(mctx.UserID)
	case "list_episodes":
		return s.handleListEpisodes(mctx.UserID)
	}

	if strings.TrimSpace(toolCall.Arguments) == "" {
		return "", fmt.Errorf("empty arguments for tool call: %s", toolCall.Name)
	}

	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(toolCall.Arguments), &probe); err != nil {
		return "", fmt.Errorf("invalid JSON arguments for %s: %w - arguments: %s", toolCall.Name, err, toolCall.Arguments)
	}

	switch toolCall.Name {
	case "save_memory":
		return s.handleSaveMemory(mctx, toolCall.Arguments)
	case "get_memory":
		return s.handleGetMemory(mctx.UserID, toolCall.Arguments)
	case "delete_memory":
		return s.handleDeleteMemory(mctx.UserID, toolCall.Arguments)
	case "save_fact":
		return s.handleSaveFact(ctx, mctx, toolCall.Arguments)
	case "forget_about":
		return s.handleForgetAbout(mctx.UserID, toolCall.Arguments)
	case "forget_episode":
		return s.handleForgetEpisode(mctx.UserID, toolCall.Arguments)
	default:
		return "", fmt.Errorf("unknown tool call: %s", toolCall.Name)
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
	if hint, err := ValidatePreference(args.Key, args.Content); err != nil {
		slog.Warn("Rejected invalid preference write", "key", args.Key, "value", args.Content, "error", err)
		return hint, nil
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

func (s *MemoryService) handleListEpisodes(userID int64) (string, error) {
	episodes, err := s.memoryManager.ListEpisodes(userID)
	if err != nil {
		return "", err
	}
	if len(episodes) == 0 {
		return "No episodes stored.", nil
	}
	var b strings.Builder
	b.WriteString("Episodes:\n")
	for _, e := range episodes {
		date := time.Unix(e.EndedAt, 0).Format("2006-01-02")
		if e.EndedAt == 0 {
			date = time.Unix(e.CreatedAt, 0).Format("2006-01-02")
		}
		fmt.Fprintf(&b, "- ID %d (%s): %s\n", e.ID, date, e.Summary)
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

func (s *MemoryService) handleForgetEpisode(userID int64, arguments string) (string, error) {
	var args struct {
		EpisodeID int64 `json:"episode_id"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for forget_episode: %w", err)
	}
	if args.EpisodeID == 0 {
		return "episode_id is required", nil
	}
	if err := s.memoryManager.DeleteEpisode(userID, args.EpisodeID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Episode deleted: %d", args.EpisodeID), nil
}
