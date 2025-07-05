package services

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type MemoryRepo interface {
	SaveMemory(userID int64, key, value string) error
	GetMemory(userID int64, key string) (*models.Memory, error)
	GetUserMemories(userID int64) ([]models.Memory, error)
	DeleteMemory(userID int64, key string) error
}

type MemoryService struct {
	memoryRepo MemoryRepo
}

func NewMemoryService(memoryRepo MemoryRepo) *MemoryService {
	return &MemoryService{memoryRepo: memoryRepo}
}

func (s *MemoryService) GetMemoryTools() []openai.Tool {
	return []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "save_memory",
				Description: "Remember a fact or an information about the user. Use this when you remember something new about the user.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{
							"type":        "string",
							"description": "A short, descriptive key for the memory (e.g., 'name', 'job', 'hobbies', 'preferences', 'facts', 'events', 'incidents' or anything else you think is important)",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The content of the memory record about the user",
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
				Description: "Retrieve a specific memory about the user using its key.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{
							"type":        "string",
							"description": "The key of the memory to retrieve",
						},
					},
					"required": []string{"key"},
				},
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "list_memories",
				Description: "List all memories about the user.",
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
				Description: "Delete a specific memory about the user.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{
							"type":        "string",
							"description": "The key of the memory to delete",
						},
					},
					"required": []string{"key"},
				},
			},
		},
	}
}

func (s *MemoryService) HandleToolCall(userID int64, toolCall openai.ToolCall) (string, error) {
	// Special handling for list_memories which doesn't need arguments
	if toolCall.Function.Name == "list_memories" {
		return s.handleListMemories(userID)
	}

	// Validate that we have complete arguments for other tool calls
	if strings.TrimSpace(toolCall.Function.Arguments) == "" {
		return "", fmt.Errorf("empty arguments for tool call: %s", toolCall.Function.Name)
	}

	// Try to validate JSON structure
	var test map[string]interface{}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &test); err != nil {
		return "", fmt.Errorf("invalid JSON arguments for %s: %w - arguments: %s", toolCall.Function.Name, err, toolCall.Function.Arguments)
	}

	switch toolCall.Function.Name {
	case "save_memory":
		return s.handleSaveMemory(userID, toolCall.Function.Arguments)
	case "get_memory":
		return s.handleGetMemory(userID, toolCall.Function.Arguments)
	case "delete_memory":
		return s.handleDeleteMemory(userID, toolCall.Function.Arguments)
	default:
		return "", fmt.Errorf("unknown tool call: %s", toolCall.Function.Name)
	}
}

func (s *MemoryService) handleSaveMemory(userID int64, arguments string) (string, error) {
	var args struct {
		Key     string `json:"key"`
		Content string `json:"content"`
	}

	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for save_memory: %w", err)
	}

	err := s.memoryRepo.SaveMemory(userID, args.Key, args.Content)
	if err != nil {
		slog.Error("Failed to save memory", "error", err, "user_id", userID)
		return "Failed to save memory", err
	}

	return fmt.Sprintf("Memory saved: %s = %s", args.Key, args.Content), nil
}

func (s *MemoryService) handleGetMemory(userID int64, arguments string) (string, error) {
	var args struct {
		Key string `json:"key"`
	}

	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for get_memory: %w", err)
	}

	memory, err := s.memoryRepo.GetMemory(userID, args.Key)
	if err != nil {
		return "", err
	}

	if memory == nil {
		return fmt.Sprintf("No memory found for key: %s", args.Key), nil
	}

	return fmt.Sprintf("Memory for '%s': %s", memory.MemoryKey, memory.MemoryValue), nil
}

func (s *MemoryService) handleListMemories(userID int64) (string, error) {
	memories, err := s.memoryRepo.GetUserMemories(userID)
	if err != nil {
		return "", err
	}

	if len(memories) == 0 {
		return "No memories found for this user", nil
	}

	var result strings.Builder
	result.WriteString("User memories:\n")
	for _, memory := range memories {
		result.WriteString(fmt.Sprintf("- %s: %s\n",
			memory.MemoryKey, memory.MemoryValue))
	}

	return result.String(), nil
}

func (s *MemoryService) handleDeleteMemory(userID int64, arguments string) (string, error) {
	var args struct {
		Key string `json:"key"`
	}

	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for delete_memory: %w", err)
	}

	err := s.memoryRepo.DeleteMemory(userID, args.Key)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Memory deleted: %s", args.Key), nil
}

func (s *MemoryService) GetMemoryContext(userID int64) string {
	memories, err := s.memoryRepo.GetUserMemories(userID)
	if err != nil || len(memories) == 0 {
		return ""
	}

	var context strings.Builder
	context.WriteString("What you know about this user:\n")

	for _, memory := range memories {
		// Only include high-importance memories in context to save tokens
		context.WriteString(fmt.Sprintf("- %s: %s\n", memory.MemoryKey, memory.MemoryValue))
	}

	return context.String()
}
