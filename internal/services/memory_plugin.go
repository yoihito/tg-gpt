package services

import (
	"context"

	"vadimgribanov.com/tg-gpt/internal/llm"
)

// MemoryPlugin composes TraceStore and Retriever into the memory side effects every
// turn needs — retrieval before the model is called and trace persistence after — so a
// single instance can be shared across every Runner/Definition that wants them (the
// same singleton-service pattern already used elsewhere in this package). Post-turn
// consolidation is deliberately NOT here — see ConsolidationPlugin — since it's only
// appropriate for turns driven by something the user actually said, and attaching
// plugins per-Runner (not per-Definition) is how a scheduled action's turn gets trace
// journaling and retrieval without also triggering fact/preference extraction from its
// own synthetic "[Scheduled reminder action triggered]" input.
type MemoryPlugin struct {
	trace     *TraceStore
	retriever *Retriever
}

func NewMemoryPlugin(trace *TraceStore, retriever *Retriever) *MemoryPlugin {
	return &MemoryPlugin{trace: trace, retriever: retriever}
}

func (p *MemoryPlugin) Name() string { return "memory" }

var (
	_ BeforeTurnHook     = (*MemoryPlugin)(nil)
	_ AfterModelCallHook = (*MemoryPlugin)(nil)
	_ AfterToolCallHook  = (*MemoryPlugin)(nil)
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

// ConsolidationPlugin triggers post-turn fact/preference extraction from what the user
// said. Split out from MemoryPlugin so it can be attached only to the chat Runner — a
// scheduled action's input is a synthetic system-generated message, not something the
// user said, so extracting "facts" from it would just be noise and wasted model calls.
type ConsolidationPlugin struct {
	consolidator *MemoryConsolidator
}

func NewConsolidationPlugin(consolidator *MemoryConsolidator) *ConsolidationPlugin {
	return &ConsolidationPlugin{consolidator: consolidator}
}

func (p *ConsolidationPlugin) Name() string { return "consolidation" }

var _ AfterTurnHook = (*ConsolidationPlugin)(nil)

func (p *ConsolidationPlugin) AfterTurn(ctx context.Context, rc *RunContext, finalResponse string) {
	queryText := joinUserInputText(rc.Inputs)
	go p.consolidator.EndTurn(context.WithoutCancel(ctx), rc.TurnCtx, queryText, finalResponse)
}
