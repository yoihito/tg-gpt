package adapters

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

const openaiStreamTimeout = 2 * time.Minute

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
	ctx, cancel := context.WithTimeout(ctx, openaiStreamTimeout)
	slog.InfoContext(ctx, "OpenAI responses stream: starting",
		"model", request.Model,
		"messages", len(request.Messages),
		"tools", len(request.Tools),
		"timeout", openaiStreamTimeout.String(),
	)
	stream := a.client.Responses.NewStreaming(ctx, toOpenAIResponseRequest(request))
	if err := stream.Err(); err != nil {
		cancel()
		slog.ErrorContext(ctx, "OpenAI responses stream: start failed", "error", err)
		return nil, err
	}
	slog.InfoContext(ctx, "OpenAI responses stream: started",
		"model", request.Model,
		"messages", len(request.Messages),
		"tools", len(request.Tools),
	)
	return &OpenaiStreamAdapter{ctx: ctx, stream: stream, cancel: cancel}, nil
}

func toOpenAIResponseRequest(request llm.Request) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(request.Model),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: toOpenAIResponseInput(request.Messages),
		},
		Tools:             toOpenAIResponseTools(request.Tools),
		ParallelToolCalls: openai.Bool(false),
		Store:             openai.Bool(false),
		Reasoning: shared.ReasoningParam{
			Effort: shared.ReasoningEffortLow,
		},
	}
	return params
}

type OpenaiStreamAdapter struct {
	ctx      context.Context
	stream   responseStream
	cancel   context.CancelFunc
	pending  []llm.StreamEvent
	current  llm.StreamEvent
	err      error
	sentText bool
}

type responseStream interface {
	Next() bool
	Current() responses.ResponseStreamEventUnion
	Err() error
	Close() error
}

func (a *OpenaiStreamAdapter) Next() bool {
	if len(a.pending) > 0 {
		a.current = a.pending[0]
		a.pending = a.pending[1:]
		return true
	}
	for a.stream.Next() {
		event := a.stream.Current()
		slog.DebugContext(a.ctx, "OpenAI responses stream: event", "type", event.Type)
		if converted, ok := a.fromResponseStreamEvent(event); ok {
			a.current = converted
			return true
		}
		if event.Type == "error" {
			a.err = fmt.Errorf("openai response stream error: %s", event.Message)
			return false
		}
	}
	a.err = a.stream.Err()
	if a.err != nil {
		slog.ErrorContext(a.ctx, "OpenAI responses stream: ended with error", "error", a.err)
	} else {
		slog.InfoContext(a.ctx, "OpenAI responses stream: ended")
	}
	return false
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
	err := a.stream.Close()
	if a.cancel != nil {
		a.cancel()
	}
	return err
}

func toOpenAIResponseInput(messages []llm.Message) responses.ResponseInputParam {
	out := make(responses.ResponseInputParam, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleTool:
			if msg.ToolResult == nil {
				continue
			}
			output := msg.ToolResult.Output
			if output == "" {
				output = " "
			}
			out = append(out, responses.ResponseInputItemParamOfFunctionCallOutput(msg.ToolResult.CallID, output))
		case llm.RoleAssistant:
			if msg.Content != "" {
				out = append(out, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleAssistant))
			}
			for _, call := range msg.ToolCalls {
				out = append(out, responses.ResponseInputItemParamOfFunctionCall(call.Arguments, call.ID, call.Name))
			}
		case llm.RoleSystem:
			out = append(out, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleSystem))
		default:
			if len(msg.Parts) > 0 {
				out = append(out, responses.ResponseInputItemParamOfMessage(toOpenAIResponseContent(msg.Parts), responses.EasyInputMessageRoleUser))
			} else {
				out = append(out, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleUser))
			}
		}
	}
	return out
}

func toOpenAIResponseContent(parts []llm.ContentPart) responses.ResponseInputMessageContentListParam {
	out := make(responses.ResponseInputMessageContentListParam, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case llm.ContentPartImageURL:
			image := responses.ResponseInputContentParamOfInputImage(responses.ResponseInputImageDetailLow)
			image.OfInputImage.ImageURL = openai.String(part.ImageURL)
			out = append(out, image)
		default:
			out = append(out, responses.ResponseInputContentParamOfInputText(part.Text))
		}
	}
	return out
}

func toOpenAIResponseTools(tools []llm.Tool) []responses.ToolUnionParam {
	out := make([]responses.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		function := responses.ToolParamOfFunction(tool.Name, tool.Parameters, tool.Strict)
		if function.OfFunction != nil && tool.Description != "" {
			function.OfFunction.Description = openai.String(tool.Description)
		}
		out = append(out, function)
	}
	return out
}

func (a *OpenaiStreamAdapter) fromResponseStreamEvent(event responses.ResponseStreamEventUnion) (llm.StreamEvent, bool) {
	switch event.Type {
	case "response.output_text.delta":
		a.sentText = true
		return llm.StreamEvent{TextDelta: event.Delta}, true
	case "response.output_item.done":
		done := event.AsResponseOutputItemDone()
		if done.Item.Type != "function_call" {
			return llm.StreamEvent{}, false
		}
		return llm.StreamEvent{
			ToolCall: &llm.ToolCall{
				ID:        done.Item.CallID,
				Index:     int(done.OutputIndex),
				Name:      done.Item.Name,
				Arguments: done.Item.Arguments.OfString,
			},
		}, true
	case "response.completed":
		completed := event.AsResponseCompleted()
		usage := llm.StreamEvent{
			Usage: &llm.Usage{
				InputTokens:       completed.Response.Usage.InputTokens,
				CachedInputTokens: completed.Response.Usage.InputTokensDetails.CachedTokens,
				OutputTokens:      completed.Response.Usage.OutputTokens,
			},
			Done: true,
		}
		if !a.sentText {
			if text := completed.Response.OutputText(); text != "" {
				a.pending = append(a.pending, usage)
				a.sentText = true
				return llm.StreamEvent{TextDelta: text}, true
			}
		}
		return usage, true
	case "response.failed":
		failed := event.AsResponseFailed()
		a.err = fmt.Errorf("openai response failed: %s", failed.Response.Error.Message)
		return llm.StreamEvent{}, false
	case "response.incomplete":
		incomplete := event.AsResponseIncomplete()
		a.err = fmt.Errorf("openai response incomplete: %s", incomplete.Response.IncompleteDetails.Reason)
		return llm.StreamEvent{}, false
	}
	return llm.StreamEvent{}, false
}
