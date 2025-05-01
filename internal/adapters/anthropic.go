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

func (a *AnthropicAdapter) Provider() string {
	return "anthropic"
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
		Model:     request.Model,
		Messages:  anthropicMessages,
		MaxTokens: 4096,
	})

	if err != nil {
		return nil, err
	}

	return &AnthropicStreamAdapter{stream: stream}, nil
}

type AnthropicStreamAdapter struct {
	stream  *anthropic.StreamedResponse
	current openai.ChatCompletionStreamResponse
	err     error
}

func (a *AnthropicStreamAdapter) Next() bool {
	resp, err := a.stream.Recv()
	if err != nil {
		a.err = err
		return false
	}
	switch typedData := resp.(type) {
	case anthropic.ContentBlockDeltaData:
		a.current = openai.ChatCompletionStreamResponse{
			Choices: []openai.ChatCompletionStreamChoice{
				{
					Delta: openai.ChatCompletionStreamChoiceDelta{Content: typedData.Delta.Text},
				},
			},
		}
	case anthropic.MessageDeltaData:
		a.current = openai.ChatCompletionStreamResponse{
			Usage: &openai.Usage{
				PromptTokens:     0,
				CompletionTokens: typedData.Usage.OutputTokens,
				TotalTokens:      0,
			},
		}
	case anthropic.MessageStartData:
		a.current = openai.ChatCompletionStreamResponse{
			Usage: &openai.Usage{
				PromptTokens:     typedData.Message.Usage.InputTokens,
				CompletionTokens: 0,
				TotalTokens:      0,
			},
		}
	}
	return true
}

func (a *AnthropicStreamAdapter) Current() openai.ChatCompletionStreamResponse {
	return a.current
}

func (a *AnthropicStreamAdapter) Err() error {
	return a.err
}

func (a *AnthropicStreamAdapter) Close() error {
	if a.stream == nil {
		return nil
	}
	a.stream.Close()
	return nil
}
