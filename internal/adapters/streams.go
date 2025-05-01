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
}

func (s *StreamAccumulator) AddChunk(chunk openai.ChatCompletionStreamResponse) {
	if chunk.Choices != nil && len(chunk.Choices) > 0 {
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
