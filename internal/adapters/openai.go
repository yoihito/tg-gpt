package adapters

import (
	"context"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

type OpenaiAdapter struct {
	client *openai.Client
}

func NewOpenaiAdapter(client *openai.Client) *OpenaiAdapter {
	return &OpenaiAdapter{client: client}
}

func (a *OpenaiAdapter) Provider() llm.Provider {
	return llm.ProviderOpenAI
}

func (a *OpenaiAdapter) Capabilities(model string) llm.Capabilities {
	return llm.Capabilities{
		FunctionTools: true,
		Vision:        true,
		ToolChoice:    true,
	}
}

func (a *OpenaiAdapter) Stream(ctx context.Context, request llm.Request) (llm.Stream, error) {
	openaiReq := openai.ChatCompletionRequest{
		Model:         request.Model,
		Messages:      toOpenAIChatMessages(request.Messages),
		Tools:         toOpenAITools(request.Tools),
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
	}
	if request.ToolChoice != "" {
		openaiReq.ToolChoice = string(request.ToolChoice)
	}

	stream, err := a.client.CreateChatCompletionStream(ctx, openaiReq)
	if err != nil {
		return nil, err
	}
	return &OpenaiStreamAdapter{stream: stream}, nil
}

type OpenaiStreamAdapter struct {
	stream  *openai.ChatCompletionStream
	current llm.StreamEvent
	err     error
}

func (a *OpenaiStreamAdapter) Next() bool {
	response, err := a.stream.Recv()
	if err != nil {
		a.err = err
		return false
	}
	a.current = fromOpenAIStreamResponse(response)
	return true
}

func (a *OpenaiStreamAdapter) Event() llm.StreamEvent {
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

func toOpenAIChatMessages(messages []llm.Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleTool:
			if msg.ToolResult == nil {
				continue
			}
			content := msg.ToolResult.Output
			if content == "" {
				content = " "
			}
			out = append(out, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				ToolCallID: msg.ToolResult.CallID,
				Content:    content,
			})
		case llm.RoleAssistant:
			content := msg.Content
			if content == "" && len(msg.ToolCalls) > 0 {
				content = " "
			}
			out = append(out, openai.ChatCompletionMessage{
				Role:      openai.ChatMessageRoleAssistant,
				Content:   content,
				ToolCalls: toOpenAIToolCalls(msg.ToolCalls),
			})
		case llm.RoleSystem:
			out = append(out, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleSystem,
				Content: msg.Content,
			})
		default:
			oai := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: msg.Content}
			if len(msg.Parts) > 0 {
				oai.Content = ""
				oai.MultiContent = toOpenAIMessageParts(msg.Parts)
			}
			out = append(out, oai)
		}
	}
	return out
}

func toOpenAIMessageParts(parts []llm.ContentPart) []openai.ChatMessagePart {
	out := make([]openai.ChatMessagePart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case llm.ContentPartImageURL:
			out = append(out, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{
					URL:    part.ImageURL,
					Detail: openai.ImageURLDetailLow,
				},
			})
		default:
			out = append(out, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeText,
				Text: part.Text,
			})
		}
	}
	return out
}

func toOpenAITools(tools []llm.Tool) []openai.Tool {
	out := make([]openai.Tool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		})
	}
	return out
}

func toOpenAIToolCalls(calls []llm.ToolCall) []openai.ToolCall {
	out := make([]openai.ToolCall, 0, len(calls))
	for _, call := range calls {
		index := call.Index
		out = append(out, openai.ToolCall{
			ID:    call.ID,
			Type:  openai.ToolTypeFunction,
			Index: &index,
			Function: openai.FunctionCall{
				Name:      call.Name,
				Arguments: call.Arguments,
			},
		})
	}
	return out
}

func fromOpenAIStreamResponse(response openai.ChatCompletionStreamResponse) llm.StreamEvent {
	var event llm.StreamEvent
	if len(response.Choices) > 0 {
		delta := response.Choices[0].Delta
		event.TextDelta = delta.Content
		if len(delta.ToolCalls) > 0 {
			event.ToolCalls = make([]llm.ToolCall, 0, len(delta.ToolCalls))
			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				event.ToolCalls = append(event.ToolCalls, llm.ToolCall{
					ID:        tc.ID,
					Index:     idx,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		}
	}
	if response.Usage != nil {
		event.Usage = &llm.Usage{
			InputTokens:  int64(response.Usage.PromptTokens),
			OutputTokens: int64(response.Usage.CompletionTokens),
		}
	}
	return event
}
