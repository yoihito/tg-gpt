package services

import (
	"context"
	"fmt"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

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
		return "Tool is not available in this mode.", fmt.Errorf("tool is not available in this mode: %s", call.Name)
	}
	result, err := handler(ctx, mctx, user, call)
	// Some handlers return an empty result alongside an error for failures
	// that were historically fatal to the turn (bad JSON, unknown tool). The
	// caller now feeds `result` back to the model instead of aborting, so it
	// must never be empty when there's an error to explain.
	if err != nil && result == "" {
		result = err.Error()
	}
	return result, err
}
