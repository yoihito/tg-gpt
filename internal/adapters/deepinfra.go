package adapters

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

const deepInfraStreamTimeout = 2 * time.Minute

// DeepInfraAdapter talks to DeepInfra's OpenAI-compatible Chat Completions endpoint
// (/v1/openai/chat/completions), not OpenAI's own Responses API — DeepInfra doesn't
// implement Responses, so this can't share OpenaiAdapter's request/stream conversion.
type DeepInfraAdapter struct {
	client *openai.Client
}

func NewDeepInfraAdapter(client *openai.Client) *DeepInfraAdapter {
	return &DeepInfraAdapter{client: client}
}

func (a *DeepInfraAdapter) Provider() llm.Provider {
	return llm.ProviderDeepInfra
}

// Capabilities is model-independent for now: every model this adapter is registered
// for in config/application.yaml is expected to support tool calling. Vision/ToolChoice
// support varies per open-weight model on DeepInfra, so callers that need per-model
// accuracy should narrow this once specific models are wired up.
func (a *DeepInfraAdapter) Capabilities(model string) llm.Capabilities {
	return llm.Capabilities{
		FunctionTools: true,
		ToolChoice:    true,
	}
}

func (a *DeepInfraAdapter) Stream(ctx context.Context, request llm.Request) (llm.Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, deepInfraStreamTimeout)
	params := toDeepInfraChatParams(request)
	slog.InfoContext(ctx, "DeepInfra chat stream: starting",
		"model", request.Model,
		"messages", len(request.Messages),
		"tools", len(request.Tools),
		"timeout", deepInfraStreamTimeout.String(),
	)
	stream := a.client.Chat.Completions.NewStreaming(ctx, params)
	if err := stream.Err(); err != nil {
		cancel()
		slog.ErrorContext(ctx, "DeepInfra chat stream: start failed", "error", err)
		return nil, err
	}
	slog.InfoContext(ctx, "DeepInfra chat stream: started", "model", request.Model)
	return &DeepInfraStreamAdapter{ctx: ctx, stream: stream, cancel: cancel}, nil
}

func toDeepInfraChatParams(request llm.Request) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(request.Model),
		Messages: toDeepInfraMessages(request.Messages),
		Tools:    toDeepInfraTools(request.Tools),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: param.NewOpt(true),
		},
	}
	if len(params.Tools) > 0 {
		params.ParallelToolCalls = param.NewOpt(false)
	}
	return params
}

func toDeepInfraMessages(messages []llm.Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleTool:
			if msg.ToolResult == nil {
				continue
			}
			out = append(out, openai.ToolMessage(msg.ToolResult.Output, msg.ToolResult.CallID))
		case llm.RoleAssistant:
			var assistant openai.ChatCompletionAssistantMessageParam
			if msg.Content != "" {
				assistant.Content.OfString = param.NewOpt(msg.Content)
			}
			for _, call := range msg.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      call.Name,
							Arguments: call.Arguments,
						},
					},
				})
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		case llm.RoleSystem:
			out = append(out, openai.SystemMessage(msg.Content))
		default:
			if len(msg.Parts) > 0 {
				out = append(out, openai.UserMessage(toDeepInfraContentParts(msg.Parts)))
			} else {
				out = append(out, openai.UserMessage(msg.Content))
			}
		}
	}
	return out
}

func toDeepInfraContentParts(parts []llm.ContentPart) []openai.ChatCompletionContentPartUnionParam {
	out := make([]openai.ChatCompletionContentPartUnionParam, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case llm.ContentPartImageURL:
			out = append(out, openai.ChatCompletionContentPartUnionParam{
				OfImageURL: &openai.ChatCompletionContentPartImageParam{
					ImageURL: openai.ChatCompletionContentPartImageImageURLParam{URL: part.ImageURL},
				},
			})
		default:
			out = append(out, openai.ChatCompletionContentPartUnionParam{
				OfText: &openai.ChatCompletionContentPartTextParam{Text: part.Text},
			})
		}
	}
	return out
}

func toDeepInfraTools(tools []llm.Tool) []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        tool.Name,
			Description: param.NewOpt(tool.Description),
			Parameters:  shared.FunctionParameters(tool.Parameters),
			Strict:      param.NewOpt(tool.Strict),
		}))
	}
	return out
}

type DeepInfraStreamAdapter struct {
	ctx     context.Context
	stream  chatCompletionChunkStream
	cancel  context.CancelFunc
	pending []llm.StreamEvent
	current llm.StreamEvent
	err     error
}

// chatCompletionChunkStream narrows *ssestream.Stream[openai.ChatCompletionChunk] down
// to what this adapter needs, the same way OpenaiAdapter's responseStream interface
// does for the Responses stream — keeps the type testable without a live HTTP stream.
type chatCompletionChunkStream interface {
	Next() bool
	Current() openai.ChatCompletionChunk
	Err() error
	Close() error
}

func (a *DeepInfraStreamAdapter) Next() bool {
	if len(a.pending) > 0 {
		a.current = a.pending[0]
		a.pending = a.pending[1:]
		return true
	}
	for a.stream.Next() {
		chunk := a.stream.Current()
		slog.DebugContext(a.ctx, "DeepInfra chat stream: chunk", "id", chunk.ID)
		events := fromChatCompletionChunk(chunk)
		if len(events) == 0 {
			continue
		}
		a.current = events[0]
		a.pending = events[1:]
		return true
	}
	a.err = a.stream.Err()
	if a.err != nil {
		slog.ErrorContext(a.ctx, "DeepInfra chat stream: ended with error", "error", a.err)
	} else {
		slog.InfoContext(a.ctx, "DeepInfra chat stream: ended")
	}
	return false
}

// fromChatCompletionChunk converts one wire chunk into zero or more StreamEvents.
// Tool call deltas are forwarded as-is (partial ID/Name/Arguments fragments keyed by
// Index) rather than accumulated here — adapters.StreamAccumulator.addToolCallDelta
// (internal/adapters/streams.go) already merges fragments by Index for every provider,
// same as it does for the OpenAI Responses adapter's fully-formed per-call events.
func fromChatCompletionChunk(chunk openai.ChatCompletionChunk) []llm.StreamEvent {
	var events []llm.StreamEvent
	if len(chunk.Choices) > 0 {
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			events = append(events, llm.StreamEvent{TextDelta: delta.Content})
		}
		for _, tc := range delta.ToolCalls {
			events = append(events, llm.StreamEvent{
				ToolCall: &llm.ToolCall{
					ID:        tc.ID,
					Index:     int(tc.Index),
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
	}
	if chunk.Usage.JSON.PromptTokens.Valid() {
		events = append(events, llm.StreamEvent{
			Usage: &llm.Usage{
				InputTokens:       chunk.Usage.PromptTokens,
				CachedInputTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
				OutputTokens:      chunk.Usage.CompletionTokens,
			},
			Done: true,
		})
	}
	return events
}

func (a *DeepInfraStreamAdapter) Event() llm.StreamEvent {
	return a.current
}

func (a *DeepInfraStreamAdapter) Err() error {
	if a.err != nil {
		return fmt.Errorf("deepinfra chat stream: %w", a.err)
	}
	return nil
}

func (a *DeepInfraStreamAdapter) Close() error {
	if a.stream == nil {
		return nil
	}
	err := a.stream.Close()
	if a.cancel != nil {
		a.cancel()
	}
	return err
}
