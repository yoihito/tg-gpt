package services

import (
	"context"
	"encoding/json"
	"fmt"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

// Definition describes one agent: its model, system prompt, and tools. It carries no
// plugins — those are fixed on whichever Runner executes it (see AsTool for how that
// lets the same Definition get different side effects depending on which Runner runs it).
type Definition struct {
	Name              string
	Model             string
	BuildSystemPrompt func(ctx context.Context) (string, error)
	Tools             *ToolSet
	// MaxIterations overrides the package default (maxToolIterations) when > 0.
	MaxIterations int
}

// SubAgentToolOptions configures the tool AsTool exposes for a sub-agent Definition.
type SubAgentToolOptions struct {
	// ToolName defaults to "agent_" + def.Name when empty.
	ToolName string
	// Description is required: it's the only thing the parent model sees to decide
	// when to delegate to this sub-agent.
	Description string
}

type subAgentArgs struct {
	Task string `json:"task"`
}

// AsTool wraps def as a callable tool: the returned handler runs def as a nested turn
// through runner, passing the tool call's "task" argument as the sub-agent's only input
// message, and returns the sub-agent's final text as the tool result. This is how one
// Definition delegates to another — there is no separate "subagent" type, just an agent
// used as a tool.
//
// Which Runner is passed in decides the sub-agent's plugin behavior (memory persistence,
// usage tracking, etc.) — Definition itself carries no plugins. A caller that wants the
// sub-agent's exchange excluded from the main dialog trace should pass a Runner built
// without a memory plugin; passing the same Runner as the parent gives it identical
// side effects to any other turn.
func AsTool(def Definition, runner *Runner, opts SubAgentToolOptions) (llm.Tool, ToolHandler) {
	toolName := opts.ToolName
	if toolName == "" {
		toolName = "agent_" + def.Name
	}

	toolDef := llm.Tool{
		Name:        toolName,
		Description: opts.Description,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "The task or question for the sub-agent to handle.",
				},
			},
			"required": []string{"task"},
		},
	}

	handler := func(ctx context.Context, mctx TurnContext, user models.User, call llm.ToolCall) (string, error) {
		var args subAgentArgs
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid arguments for %s: %w", toolName, err)
		}

		stream, err := runner.Run(ctx, def, user, mctx, []UserInput{
			{Message: llm.Message{Role: llm.RoleUser, Content: args.Task}},
		}, RunOptions{})
		if err != nil {
			return "", err
		}
		for stream.Next() {
			// Sub-agents aren't streamed to the delivery layer directly — the parent's
			// own RunStream is what callers watch — so just drain to completion here.
		}
		if err := stream.Err(); err != nil {
			return "", err
		}
		return stream.Result().Response, nil
	}

	return toolDef, handler
}
