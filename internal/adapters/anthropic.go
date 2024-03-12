package adapters

import (
	"context"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/vendors/anthropic"
)

type AnthropicAdapter struct {
	client *anthropic.Client
}

func NewAnthropicAdapter(client *anthropic.Client) *AnthropicAdapter {
	return &AnthropicAdapter{client: client}
}

func (a *AnthropicAdapter) CreateChatCompletionStream(ctx context.Context, request openai.ChatCompletionRequest) (LLMStream, error) {
	anthropicMessages := []anthropic.Message{}
	systemPrompt := ""
	for _, message := range request.Messages {
		if message.Role == "system" {
			systemPrompt = message.Content
			continue
		}
		anthropicMessages = append(anthropicMessages, anthropic.Message{
			Role:    message.Role,
			Content: message.Content,
		})
	}

	stream, err := a.client.CreateMessagesStream(ctx, anthropic.CreateMessageRequest{
		System:    systemPrompt,
		Model:     "claude-3-opus-20240229",
		Messages:  anthropicMessages,
		MaxTokens: 4096,
	})

	if err != nil {
		return nil, err
	}

	return &AnthropicStreamAdapter{stream: stream}, nil
}

type AnthropicStreamAdapter struct {
	stream              *anthropic.StreamedResponse
	accumulatedResponse string
}

func (a *AnthropicStreamAdapter) Recv() (openai.ChatCompletionStreamResponse, error) {
	resp, err := a.stream.Recv()
	if err != nil {
		return openai.ChatCompletionStreamResponse{}, err
	}

	a.accumulatedResponse += resp.Delta.Text
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{
			{
				Delta: openai.ChatCompletionStreamChoiceDelta{
					Content: resp.Delta.Text,
				},
			},
		},
	}, nil
}

func (a *AnthropicStreamAdapter) InputTokens() int64 {
	return 0
}

func (a *AnthropicStreamAdapter) OutputTokens() (int, error) {
	return 0, nil
}

func (a *AnthropicStreamAdapter) AccumulatedResponse() string {
	return a.accumulatedResponse
}

func (a *AnthropicStreamAdapter) Close() {
	a.stream.Close()
}
