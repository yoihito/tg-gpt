package services

import (
	"context"

	"vadimgribanov.com/tg-gpt/internal/llm"
)

// MemoryPlugin composes TraceStore, Retriever, and MemoryConsolidator into the memory
// side effects every turn needs — retrieval before the model is called, trace persistence
// after, and post-turn consolidation — so a single instance can be shared across every
// Runner/Definition that wants them (the same singleton-service pattern already used
// elsewhere in this package).
type MemoryPlugin struct {
	trace        *TraceStore
	retriever    *Retriever
	consolidator *MemoryConsolidator
}

func NewMemoryPlugin(trace *TraceStore, retriever *Retriever, consolidator *MemoryConsolidator) *MemoryPlugin {
	return &MemoryPlugin{trace: trace, retriever: retriever, consolidator: consolidator}
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
	retrieved, err := p.retriever.Retrieve(ctx, rc.TurnCtx, queryText)
	if err != nil {
		return err
	}
	history := p.retriever.AssemblePrompt(rc.SystemPrompt, retrieved)
	history = appendMissingCurrentInputs(history, retrieved.RecentTrace, rc.Inputs)
	rc.History = p.retriever.AppendRetrievalContext(history, retrieved)
	return nil
}

func (p *MemoryPlugin) AfterModelCall(ctx context.Context, rc *RunContext, content string, toolCalls []llm.ToolCall, usage llm.Usage) error {
	_, err := p.trace.AppendModelMsg(rc.TurnCtx, content, toolCalls, rc.Model, 0)
	return err
}

func (p *MemoryPlugin) AfterToolCall(ctx context.Context, rc *RunContext, call llm.ToolCall, result string, toolErr error) error {
	_, err := p.trace.AppendToolResult(rc.TurnCtx, call.ID, call.Name, result)
	return err
}

func (p *MemoryPlugin) AfterTurn(ctx context.Context, rc *RunContext, finalResponse string) {
	queryText := joinUserInputText(rc.Inputs)
	go p.consolidator.EndTurn(context.WithoutCancel(ctx), rc.TurnCtx, queryText, finalResponse)
}
