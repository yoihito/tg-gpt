package adapters

import (
	"context"

	"github.com/sashabaranov/go-openai"
)

type OpenaiAdapter struct {
	client *openai.Client
}

func NewOpenaiAdapter(client *openai.Client) *OpenaiAdapter {
	return &OpenaiAdapter{client: client}
}

func (a *OpenaiAdapter) Provider() string {
	return "openai"
}

func (a *OpenaiAdapter) CreateChatCompletionStream(ctx context.Context, request openai.ChatCompletionRequest) (LLMStream, error) {
	stream, err := a.client.CreateChatCompletionStream(ctx, request)
	if err != nil {
		return nil, err
	}
	return &OpenaiStreamAdapter{stream: stream}, nil
}

type OpenaiStreamAdapter struct {
	stream  *openai.ChatCompletionStream
	current openai.ChatCompletionStreamResponse
	err     error
}

func (a *OpenaiStreamAdapter) Next() bool {
	response, err := a.stream.Recv()
	if err != nil {
		a.err = err
		return false
	}
	a.current = response
	return true
}

func (a *OpenaiStreamAdapter) Current() openai.ChatCompletionStreamResponse {
	return a.current
}

func (a *OpenaiStreamAdapter) Err() error {
	return a.err
}

func (a *OpenaiStreamAdapter) Close() error {
	if a.stream == nil {
		return nil
	}
	a.stream.Close()
	return nil
}
