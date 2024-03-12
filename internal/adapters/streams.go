package adapters

import "github.com/sashabaranov/go-openai"

type LLMStream interface {
	Recv() (openai.ChatCompletionStreamResponse, error)
	Close()
	OutputTokens() (int, error)
	InputTokens() int64
	AccumulatedResponse() string
}
