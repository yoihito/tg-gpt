package adapters

import (
	"context"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/vendors/anthropic"
)

type AnthropicAdapter struct {
	client *anthropic.Client
}

func NewAnthropicAdapter(client *anthropic.Client) *AnthropicAdapter {
	return &AnthropicAdapter{client: client}
}

func (a *AnthropicAdapter) Provider() llm.Provider {
	return llm.ProviderAnthropic
}

func (a *AnthropicAdapter) Capabilities(model string) llm.Capabilities {
	return llm.Capabilities{}
}

func (a *AnthropicAdapter) Stream(ctx context.Context, request llm.Request) (llm.Stream, error) {
	anthropicMessages := []anthropic.Message{}
	systemPrompt := ""
	for _, message := range request.Messages {
		if message.Role == llm.RoleSystem {
			systemPrompt = message.Content
			continue
		}
		if message.ToolResult != nil {
			continue
		}
		anthropicMessages = append(anthropicMessages, anthropic.Message{
			Role:    string(message.Role),
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
	current llm.StreamEvent
	err     error
}

func (a *AnthropicStreamAdapter) Next() bool {
	resp, err := a.stream.Recv()
	if err != nil {
		a.err = err
		return false
	}
	a.current = llm.StreamEvent{}
	switch typedData := resp.(type) {
	case anthropic.ContentBlockDeltaData:
		a.current.TextDelta = typedData.Delta.Text
	case anthropic.MessageDeltaData:
		a.current.Usage = &llm.Usage{OutputTokens: int64(typedData.Usage.OutputTokens)}
	case anthropic.MessageStartData:
		a.current.Usage = &llm.Usage{InputTokens: int64(typedData.Message.Usage.InputTokens)}
	}
	return true
}

func (a *AnthropicStreamAdapter) Event() llm.StreamEvent {
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
