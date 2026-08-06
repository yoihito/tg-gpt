package services

import (
	"context"

	"vadimgribanov.com/tg-gpt/internal/llm"
)

// MemoryPlugin wraps MemoryManager so a single instance can be shared across every
// Runner/Definition that wants memory retrieval, trace persistence, and post-turn
// extraction — the same singleton-service pattern already used elsewhere in this package.
type MemoryPlugin struct {
	mm *MemoryManager
}

func NewMemoryPlugin(mm *MemoryManager) *MemoryPlugin {
	return &MemoryPlugin{mm: mm}
}

func (p *MemoryPlugin) Name() string { return "memory" }

var (
	_ BeforeTurnHook     = (*MemoryPlugin)(nil)
	_ AfterModelCallHook = (*MemoryPlugin)(nil)
	_ AfterToolCallHook  = (*MemoryPlugin)(nil)
	_ AfterTurnHook      = (*MemoryPlugin)(nil)
)

func (p *MemoryPlugin) BeforeTurn(ctx context.Context, rc *RunContext) error {
	queryText := joinUserInputText(rc.Inputs)
	retrieved, err := p.mm.Retrieve(ctx, rc.TurnCtx, queryText)
	if err != nil {
		return err
	}
	history := p.mm.AssemblePrompt(rc.SystemPrompt, retrieved)
	rc.History = appendMissingCurrentInputs(history, retrieved.RecentTrace, rc.Inputs)
	return nil
}

func (p *MemoryPlugin) AfterModelCall(ctx context.Context, rc *RunContext, content string, toolCalls []llm.ToolCall, usage llm.Usage) error {
	_, err := p.mm.AppendModelMsg(rc.TurnCtx, content, toolCalls, rc.Model, 0)
	return err
}

func (p *MemoryPlugin) AfterToolCall(ctx context.Context, rc *RunContext, call llm.ToolCall, result string, toolErr error) error {
	_, err := p.mm.AppendToolResult(rc.TurnCtx, call.ID, call.Name, result)
	return err
}

func (p *MemoryPlugin) AfterTurn(ctx context.Context, rc *RunContext, finalResponse string) {
	queryText := joinUserInputText(rc.Inputs)
	go p.mm.EndTurn(context.WithoutCancel(ctx), rc.TurnCtx, queryText, finalResponse)
}
