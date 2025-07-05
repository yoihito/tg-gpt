package adapters

import (
	"github.com/sashabaranov/go-openai"
)

type LLMStream interface {
	Next() bool
	Current() openai.ChatCompletionStreamResponse
	Close() error
	Err() error
}

type StreamAccumulator struct {
	accumulatedResponse string
	promptTokens        int64
	completionTokens    int64
	toolCalls           map[int]openai.ToolCall
}

func NewStreamAccumulator() *StreamAccumulator {
	return &StreamAccumulator{
		toolCalls: make(map[int]openai.ToolCall),
	}
}

func (s *StreamAccumulator) AddChunk(chunk openai.ChatCompletionStreamResponse) {
	if len(chunk.Choices) > 0 {
		if len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			for _, toolCall := range chunk.Choices[0].Delta.ToolCalls {
				if _, ok := s.toolCalls[*toolCall.Index]; !ok {
					s.toolCalls[*toolCall.Index] = toolCall
					continue
				}
				existingCall := s.toolCalls[*toolCall.Index]
				existingCall.Function.Arguments += toolCall.Function.Arguments
				s.toolCalls[*toolCall.Index] = existingCall
			}
		}
		s.accumulatedResponse += chunk.Choices[0].Delta.Content
	}
	if chunk.Usage != nil {
		s.promptTokens += int64(chunk.Usage.PromptTokens)
		s.completionTokens += int64(chunk.Usage.CompletionTokens)
	}
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

func (s *StreamAccumulator) GetToolCalls() []openai.ToolCall {
	toolCalls := make([]openai.ToolCall, 0, len(s.toolCalls))
	for _, toolCall := range s.toolCalls {
		toolCalls = append(toolCalls, toolCall)
	}
	return toolCalls
}
