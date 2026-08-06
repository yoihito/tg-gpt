package services

import (
	"context"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

// Plugin is cross-cutting behavior attached to a Runner at construction time (not to a
// Definition) — it observes/mutates a turn through whichever optional hook interfaces
// below it implements. A Plugin should be stateless with respect to any single turn,
// deriving everything it needs from the RunContext passed to each hook.
type Plugin interface {
	Name() string
}

// BeforeTurnHook runs once, before the first model call, with rc.History already seeded
// with a default [system prompt, current inputs] history. A plugin can replace rc.History
// entirely (e.g. MemoryPlugin builds a richer history from retrieved memory + past trace).
type BeforeTurnHook interface {
	BeforeTurn(ctx context.Context, rc *RunContext) error
}

// BeforeModelCallHook runs once per iteration, immediately before the model is called.
type BeforeModelCallHook interface {
	BeforeModelCall(ctx context.Context, rc *RunContext) error
}

// AfterModelCallHook runs once per iteration, after the provider stream has fully
// drained. toolCalls is empty on the turn's final (no-tool-call) iteration, including the
// synthetic iteration produced when the iteration cap is hit.
type AfterModelCallHook interface {
	AfterModelCall(ctx context.Context, rc *RunContext, content string, toolCalls []llm.ToolCall, usage llm.Usage) error
}

// BeforeToolCallHook runs once per tool call, before it executes.
type BeforeToolCallHook interface {
	BeforeToolCall(ctx context.Context, rc *RunContext, call llm.ToolCall) error
}

// AfterToolCallHook runs once per tool call, after it executes. toolErr mirrors
// ToolSet.Execute's return: non-nil only for failures that made result meaningless.
type AfterToolCallHook interface {
	AfterToolCall(ctx context.Context, rc *RunContext, call llm.ToolCall, result string, toolErr error) error
}

// AfterTurnHook runs once, after the turn's final response is ready. It cannot abort
// the turn (the response is already decided) so it has no error return; a plugin that
// needs to do slow work here should do it in its own goroutine, as MemoryPlugin does.
type AfterTurnHook interface {
	AfterTurn(ctx context.Context, rc *RunContext, finalResponse string)
}

// RunContext is the mutable state threaded through one turn. Runner owns creating it and
// advancing History/Inputs between iterations; plugins read and write it from their hooks.
type RunContext struct {
	User         models.User
	TurnCtx      TurnContext
	Model        string
	Inputs       []UserInput
	History      []llm.Message
	SystemPrompt string
	Iteration    int
}
