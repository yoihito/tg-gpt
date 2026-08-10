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
)

// ValidateToolArguments checks that a tool call's raw JSON arguments are present and
// well-formed, returning an error a handler can surface as-is. Tools that take no
// arguments should skip this check rather than call it.
func ValidateToolArguments(name, arguments string) error {
	if strings.TrimSpace(arguments) == "" {
		return fmt.Errorf("empty arguments for tool call: %s", name)
	}
	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(arguments), &probe); err != nil {
		return fmt.Errorf("invalid JSON arguments for %s: %w - arguments: %s", name, err, arguments)
	}
	return nil
}

// ToolHandler executes one tool call and returns its result. Domain/validation
// failures should be encoded in the returned string (so the model can see and
// react to them); the error return is for failures that make the result
// meaningless, e.g. the tool being unavailable in the current mode.
type ToolHandler func(ctx context.Context, mctx TurnContext, user models.User, call llm.ToolCall) (string, error)

// ToolSet is a name-keyed registry of tool definitions and their handlers. It
// doubles as the allow-list for whatever mode it was built for (e.g. default
// chat vs. scheduled-action turns).
type ToolSet struct {
	defs     []llm.Tool
	handlers map[string]ToolHandler
}

func NewToolSet() *ToolSet {
	return &ToolSet{handlers: make(map[string]ToolHandler)}
}

func (s *ToolSet) Register(def llm.Tool, handler ToolHandler) {
	s.defs = append(s.defs, def)
	s.handlers[def.Name] = handler
}

func (s *ToolSet) RegisterAll(defs []llm.Tool, handler ToolHandler) {
	for _, def := range defs {
		s.Register(def, handler)
	}
}

func (s *ToolSet) Defs() []llm.Tool {
	return s.defs
}

// Lookup returns the registered definition for a tool name, if any.
func (s *ToolSet) Lookup(name string) (llm.Tool, bool) {
	for _, def := range s.defs {
		if def.Name == name {
			return def, true
		}
	}
	return llm.Tool{}, false
}

func (s *ToolSet) Execute(ctx context.Context, mctx TurnContext, user models.User, call llm.ToolCall) (string, error) {
	handler, ok := s.handlers[call.Name]
	if !ok {
		slog.WarnContext(ctx, "Tool call: unavailable in this mode", "tool", call.Name)
		return "Tool is not available in this mode.", fmt.Errorf("tool is not available in this mode: %s", call.Name)
	}
	slog.InfoContext(ctx, "Tool call: starting", "tool", call.Name, "arguments", call.Arguments)
	start := time.Now()
	result, err := handler(ctx, mctx, user, call)
	duration := time.Since(start)
	if err != nil {
		slog.WarnContext(ctx, "Tool call: failed", "tool", call.Name, "duration", duration.String(), "error", err)
	} else {
		slog.InfoContext(ctx, "Tool call: completed", "tool", call.Name, "duration", duration.String())
	}
	// Some handlers return an empty result alongside an error for failures
	// that were historically fatal to the turn (bad JSON, unknown tool). The
	// caller now feeds `result` back to the model instead of aborting, so it
	// must never be empty when there's an error to explain.
	if err != nil && result == "" {
		result = err.Error()
	}
	return result, err
}
