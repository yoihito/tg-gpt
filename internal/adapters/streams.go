package adapters

import (
	"sort"

	"vadimgribanov.com/tg-gpt/internal/llm"
)

type StreamAccumulator struct {
	accumulatedResponse string
	promptTokens        int64
	completionTokens    int64
	toolCalls           map[int]llm.ToolCall
}

func NewStreamAccumulator() *StreamAccumulator {
	return &StreamAccumulator{
		toolCalls: make(map[int]llm.ToolCall),
	}
}

func (s *StreamAccumulator) AddEvent(event llm.StreamEvent) {
	if event.TextDelta != "" {
		s.accumulatedResponse += event.TextDelta
	}
	if event.ToolCall != nil {
		s.addToolCallDelta(*event.ToolCall)
	}
	for _, call := range event.ToolCalls {
		s.addToolCallDelta(call)
	}
	if event.Usage != nil {
		s.promptTokens += event.Usage.InputTokens
		s.completionTokens += event.Usage.OutputTokens
	}
}

func (s *StreamAccumulator) addToolCallDelta(call llm.ToolCall) {
	idx := call.Index
	existing, ok := s.toolCalls[idx]
	if !ok {
		s.toolCalls[idx] = call
		return
	}
	if call.ID != "" {
		existing.ID = call.ID
	}
	if call.Name != "" {
		existing.Name = call.Name
	}
	existing.Arguments += call.Arguments
	s.toolCalls[idx] = existing
}

func (s *StreamAccumulator) AccumulatedResponse() string {
	return s.accumulatedResponse
}

func (s *StreamAccumulator) OutputTokens() int64 {
	return s.completionTokens
}

func (s *StreamAccumulator) InputTokens() int64 {
	return s.promptTokens
}

func (s *StreamAccumulator) HasToolCalls() bool {
	return len(s.toolCalls) > 0
}

func (s *StreamAccumulator) GetToolCalls() []llm.ToolCall {
	toolCalls := make([]llm.ToolCall, 0, len(s.toolCalls))
	for _, toolCall := range s.toolCalls {
		toolCalls = append(toolCalls, toolCall)
	}
	sort.Slice(toolCalls, func(i, j int) bool {
		return toolCalls[i].Index < toolCalls[j].Index
	})
	return toolCalls
}
