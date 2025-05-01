package adapters

import (
	"fmt"
	"strings"

	"github.com/pkoukk/tiktoken-go"
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
}

func (s *StreamAccumulator) AddChunk(chunk openai.ChatCompletionStreamResponse) {
	s.accumulatedResponse += chunk.Choices[0].Delta.Content
}

func (s *StreamAccumulator) AccumulatedResponse() string {
	return s.accumulatedResponse
}

func (s *StreamAccumulator) OutputTokens() (int, error) {
	encoder, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		return 0, err
	}
	return len(encoder.Encode(s.accumulatedResponse, nil, nil)), err
}

func (s *StreamAccumulator) InputTokens() int64 {
	return 0
}

// OpenAI Cookbook: https://github.com/openai/openai-cookbook/blob/main/examples/How_to_count_tokens_with_tiktoken.ipynb
func numTokensFromMessages(messages []openai.ChatCompletionMessage, model string) (numTokens int, err error) {
	tkm, err := tiktoken.EncodingForModel(model)
	if err != nil {
		return 0, fmt.Errorf("encoding for model: %v", err)
	}

	var tokensPerMessage, tokensPerName int
	switch model {
	case "gpt-3.5-turbo-0613",
		"gpt-3.5-turbo-16k-0613",
		"gpt-4-0314",
		"gpt-4-32k-0314",
		"gpt-4-0613",
		"gpt-4-32k-0613":
		tokensPerMessage = 3
		tokensPerName = 1
	case "gpt-3.5-turbo-0301":
		tokensPerMessage = 4 // every message follows <|start|>{role/name}\n{content}<|end|>\n
		tokensPerName = -1   // if there's a name, the role is omitted
	default:
		if strings.Contains(model, "gpt-3.5-turbo") {
			return numTokensFromMessages(messages, "gpt-3.5-turbo-0613")
		} else if strings.Contains(model, "gpt-4") {
			return numTokensFromMessages(messages, "gpt-4-0613")
		} else {
			return 0, fmt.Errorf("num_tokens_from_messages() is not implemented for model %s. See https://github.com/openai/openai-python/blob/main/chatml.md for information on how messages are converted to tokens.", model)
		}
	}

	for _, message := range messages {
		numTokens += tokensPerMessage
		numTokens += len(tkm.Encode(message.Content, nil, nil))
		numTokens += len(tkm.Encode(message.Role, nil, nil))
		numTokens += len(tkm.Encode(message.Name, nil, nil))
		if message.Name != "" {
			numTokens += tokensPerName
		}
	}
	numTokens += 3 // every reply is primed with <|start|>assistant<|message|>
	return numTokens, nil
}
