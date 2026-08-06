package services

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"vadimgribanov.com/tg-gpt/internal/adapters"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

// maxToolIterations bounds how many stream+tool-call rounds a single turn can run before
// it's forced to stop. Without a cap, a model stuck repeatedly sending bad tool calls
// would loop until the context is cancelled. Definition.MaxIterations overrides this
// per-agent; 0 (the zero value) means "use this default".
const maxToolIterations = 25

const capMessage = "I hit an internal limit handling this request (too many tool calls in a row). Please try again or rephrase your request."

// Runner executes a Definition. Plugins are fixed at construction time — every Definition
// run through a given Runner gets the same cross-cutting behavior (memory, usage, etc.).
// Runner holds no per-call mutable state, so Run is safe to call concurrently or
// recursively (a sub-agent calling its own sub-agent).
type Runner struct {
	client  LLMClient
	plugins []Plugin
}

func NewRunner(client LLMClient, plugins ...Plugin) *Runner {
	return &Runner{client: client, plugins: plugins}
}

// RunOptions carries behavior that belongs to the caller's delivery mechanism, not to any
// plugin: draining more user input that arrived while the turn was in progress.
type RunOptions struct {
	DrainInputs func(ctx context.Context) ([]UserInput, error)
}

type RunEventKind int

const (
	// RunEventStreamDelta passes through a provider-level stream event (text/tool-call
	// deltas, usage) in real time, exactly as received.
	RunEventStreamDelta RunEventKind = iota
	// RunEventIterationEnd signals one model-call round finished draining (whether or not
	// it produced tool calls); callers driving a live UI should flush buffered output now.
	RunEventIterationEnd
	// RunEventToolCallStarted signals a tool call is about to execute.
	RunEventToolCallStarted
	// RunEventToolCallFinished signals a tool call finished executing.
	RunEventToolCallFinished
)

type RunEvent struct {
	Kind        RunEventKind
	StreamEvent llm.StreamEvent
	ToolCall    llm.ToolCall
	ToolResult  string
	ToolErr     error
}

type RunResult struct {
	Response     string
	InputTokens  int64
	OutputTokens int64
}

// RunStream mirrors llm.Stream's Next/Event/Err shape on purpose: it's a familiar,
// fully synchronous iterator (no goroutines/channels) — Next just does more work before
// returning when the current provider-stream segment is exhausted.
type RunStream interface {
	Next() bool
	Event() RunEvent
	Err() error
	// Result is valid once Next() returns false and Err() == nil.
	Result() RunResult
}

func (r *Runner) Run(
	ctx context.Context,
	def Definition,
	user models.User,
	mctx TurnContext,
	inputs []UserInput,
	opts RunOptions,
) (RunStream, error) {
	if def.Tools == nil {
		def.Tools = NewToolSet()
	}
	maxIter := def.MaxIterations
	if maxIter <= 0 {
		maxIter = maxToolIterations
	}
	return &runStreamImpl{
		ctx:     ctx,
		runner:  r,
		def:     def,
		opts:    opts,
		maxIter: maxIter,
		rc: &RunContext{
			User:    user,
			TurnCtx: mctx,
			Model:   def.Model,
			Inputs:  inputs,
		},
		phase: phaseStartIteration,
	}, nil
}

type runPhase int

const (
	phaseStartIteration runPhase = iota
	phaseCapDelta
	phaseStreamModel
	phaseIterationEnd
	phaseToolCallStart
	phaseToolCallFinish
	phaseDrainInputs
	phaseFinishTurn
	phaseDone
)

type runStreamImpl struct {
	ctx     context.Context
	runner  *Runner
	def     Definition
	opts    RunOptions
	maxIter int
	rc      *RunContext

	phase       runPhase
	current     llm.Stream
	accumulator *adapters.StreamAccumulator

	pendingToolCalls []llm.ToolCall
	toolIdx          int

	totalInputTokens  int64
	totalOutputTokens int64
	finalResponse     string

	event  RunEvent
	err    error
	result RunResult
}

func (s *runStreamImpl) Next() bool {
	for {
		switch s.phase {
		case phaseDone:
			return false

		case phaseStartIteration:
			s.rc.Iteration++
			if s.rc.Iteration == 1 {
				sysPrompt, err := s.def.BuildSystemPrompt(s.ctx)
				if err != nil {
					return s.fail(err)
				}
				s.rc.SystemPrompt = sysPrompt
				s.rc.History = defaultHistory(sysPrompt, s.rc.Inputs)
				for _, p := range s.runner.plugins {
					if h, ok := p.(BeforeTurnHook); ok {
						if err := h.BeforeTurn(s.ctx, s.rc); err != nil {
							return s.fail(err)
						}
					}
				}
			}

			if s.rc.Iteration > s.maxIter {
				if err := s.finalizeIteration(capMessage, nil, llm.Usage{}); err != nil {
					return s.fail(err)
				}
				s.phase = phaseCapDelta
				continue
			}

			for _, p := range s.runner.plugins {
				if h, ok := p.(BeforeModelCallHook); ok {
					if err := h.BeforeModelCall(s.ctx, s.rc); err != nil {
						return s.fail(err)
					}
				}
			}

			stream, err := s.runner.client.Stream(s.ctx, llm.Request{
				Model:      s.rc.Model,
				Messages:   s.rc.History,
				Tools:      s.def.Tools.Defs(),
				ToolChoice: llm.ToolChoiceAuto,
			})
			if err != nil {
				return s.fail(err)
			}
			s.current = stream
			s.accumulator = adapters.NewStreamAccumulator()
			s.phase = phaseStreamModel
			continue

		case phaseCapDelta:
			s.event = RunEvent{Kind: RunEventStreamDelta, StreamEvent: llm.StreamEvent{TextDelta: s.finalResponse}}
			s.phase = phaseIterationEnd
			return true

		case phaseStreamModel:
			if s.current.Next() {
				ev := s.current.Event()
				s.accumulator.AddEvent(ev)
				s.event = RunEvent{Kind: RunEventStreamDelta, StreamEvent: ev}
				return true
			}
			if closeErr := s.current.Close(); closeErr != nil {
				slog.WarnContext(s.ctx, "Error closing stream", "error", closeErr)
			}
			if err := s.current.Err(); err != nil && !errors.Is(err, io.EOF) {
				return s.fail(err)
			}
			content := s.accumulator.AccumulatedResponse()
			toolCalls := s.accumulator.GetToolCalls()
			usage := llm.Usage{InputTokens: s.accumulator.InputTokens(), OutputTokens: s.accumulator.OutputTokens()}
			if err := s.finalizeIteration(content, toolCalls, usage); err != nil {
				return s.fail(err)
			}
			s.phase = phaseIterationEnd
			continue

		case phaseIterationEnd:
			s.event = RunEvent{Kind: RunEventIterationEnd}
			if len(s.pendingToolCalls) == 0 {
				s.phase = phaseFinishTurn
			} else {
				s.phase = phaseToolCallStart
			}
			return true

		case phaseToolCallStart:
			s.event = RunEvent{Kind: RunEventToolCallStarted, ToolCall: s.pendingToolCalls[s.toolIdx]}
			s.phase = phaseToolCallFinish
			return true

		case phaseToolCallFinish:
			call := s.pendingToolCalls[s.toolIdx]
			for _, p := range s.runner.plugins {
				if h, ok := p.(BeforeToolCallHook); ok {
					if err := h.BeforeToolCall(s.ctx, s.rc, call); err != nil {
						return s.fail(err)
					}
				}
			}
			result, toolErr := s.def.Tools.Execute(s.ctx, s.rc.TurnCtx, s.rc.User, call)
			for _, p := range s.runner.plugins {
				if h, ok := p.(AfterToolCallHook); ok {
					if err := h.AfterToolCall(s.ctx, s.rc, call, result, toolErr); err != nil {
						return s.fail(err)
					}
				}
			}
			s.rc.History = append(s.rc.History, llm.Message{
				Role: llm.RoleTool,
				ToolResult: &llm.ToolResult{
					CallID: call.ID,
					Name:   call.Name,
					Output: result,
				},
			})
			s.event = RunEvent{Kind: RunEventToolCallFinished, ToolCall: call, ToolResult: result, ToolErr: toolErr}
			s.toolIdx++
			if s.toolIdx >= len(s.pendingToolCalls) {
				s.phase = phaseDrainInputs
			} else {
				s.phase = phaseToolCallStart
			}
			return true

		case phaseDrainInputs:
			if s.opts.DrainInputs != nil {
				newInputs, err := s.opts.DrainInputs(s.ctx)
				if err != nil {
					return s.fail(err)
				}
				for _, input := range newInputs {
					s.rc.Inputs = append(s.rc.Inputs, input)
					s.rc.History = append(s.rc.History, midTurnSystemMessage(input.Message))
				}
			}
			s.phase = phaseStartIteration
			continue

		case phaseFinishTurn:
			for _, p := range s.runner.plugins {
				if h, ok := p.(AfterTurnHook); ok {
					h.AfterTurn(s.ctx, s.rc, s.finalResponse)
				}
			}
			s.result = RunResult{
				Response:     s.finalResponse,
				InputTokens:  s.totalInputTokens,
				OutputTokens: s.totalOutputTokens,
			}
			s.phase = phaseDone
			return false
		}
	}
}

func (s *runStreamImpl) Event() RunEvent   { return s.event }
func (s *runStreamImpl) Err() error        { return s.err }
func (s *runStreamImpl) Result() RunResult { return s.result }

func (s *runStreamImpl) fail(err error) bool {
	s.err = err
	s.phase = phaseDone
	return false
}

// finalizeIteration runs AfterModelCallHooks and appends the assistant's message to
// history. Used for both real model-call iterations and the synthetic cap-hit iteration.
func (s *runStreamImpl) finalizeIteration(content string, toolCalls []llm.ToolCall, usage llm.Usage) error {
	s.totalInputTokens += usage.InputTokens
	s.totalOutputTokens += usage.OutputTokens
	s.finalResponse = content
	s.pendingToolCalls = toolCalls
	s.toolIdx = 0

	for _, p := range s.runner.plugins {
		if h, ok := p.(AfterModelCallHook); ok {
			if err := h.AfterModelCall(s.ctx, s.rc, content, toolCalls, usage); err != nil {
				return err
			}
		}
	}

	msg := llm.Message{Role: llm.RoleAssistant, Content: content}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	s.rc.History = append(s.rc.History, msg)
	return nil
}

func defaultHistory(systemPrompt string, inputs []UserInput) []llm.Message {
	history := []llm.Message{{Role: llm.RoleSystem, Content: systemPrompt}}
	for _, input := range inputs {
		history = append(history, input.Message)
	}
	return history
}
