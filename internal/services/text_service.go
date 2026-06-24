package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/adapters"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/telegram_utils"
)

func NewTextService(
	client LLMClient,
	usersRepo UsersRepo,
	memoryService *MemoryService,
	memoryManager *MemoryManager,
	reminderService *ReminderService,
	dialogTimeout int64,
	defaultModel string,
) *TextService {
	return &TextService{
		client:          client,
		usersRepo:       usersRepo,
		memoryService:   memoryService,
		memoryManager:   memoryManager,
		reminderService: reminderService,
		dialogTimeout:   dialogTimeout,
		defaultModel:    defaultModel,
	}
}

type TextService struct {
	client          LLMClient
	usersRepo       UsersRepo
	memoryService   *MemoryService
	memoryManager   *MemoryManager
	reminderService *ReminderService
	dialogTimeout   int64
	defaultModel    string
}

type LLMClient interface {
	CreateChatCompletionStream(ctx context.Context, request openai.ChatCompletionRequest) (adapters.LLMStream, error)
	IsClientRegistered(modelId string) bool
}

type UsersRepo interface {
	UpdateUser(user models.User) error
}

const EOFStatus = "EOF"
const AssistantPrompt = `You are a helpful assistant. Your name is Johnny. You can save things you learn about the user (preferences and facts) and create, list, or cancel reminders.

IMPORTANT:
- When creating reminders, you MUST know the user's timezone. Look in their preferences/facts below; if it isn't there, ask naturally and save it as a preference with key "timezone".
- When the user mentions travel or location changes, update the timezone.
- IT IS VERY IMPORTANT to capture all the smallest details about the user.

Today is %s. Give short concise answers.`

type Result struct {
	Status        string
	ChunkResponse openai.ChatCompletionStreamResponse
	Err           error
}

type MessagePartType string

const (
	MessagePartTypeText  MessagePartType = "text"
	MessagePartTypeImage MessagePartType = "image"
)

type StreamedResponse struct{}

func (h *TextService) RetryWithMessage(
	ctx context.Context,
	user models.User,
	tgUserMessageId int64,
	userMsg openai.ChatCompletionMessage,
	streamer *telegram_utils.TelegramStreamer,
) error {
	return h.handleLLMRequest(ctx, user, tgUserMessageId, userMsg, streamer)
}

func (h *TextService) OnStreamableTextHandler(ctx context.Context, user models.User, tgUserMessageId int64, userText string, streamer *telegram_utils.TelegramStreamer) error {
	return h.handleLLMRequest(ctx, user, tgUserMessageId, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: userText,
	}, streamer)
}

func (h *TextService) OnStreamableVisionHandler(ctx context.Context, user models.User, tgUserMessageId int64, userText string, imageUrl string, streamer *telegram_utils.TelegramStreamer) error {
	return h.handleLLMRequest(ctx, user, tgUserMessageId, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		MultiContent: []openai.ChatMessagePart{
			{
				Type: openai.ChatMessagePartTypeText,
				Text: userText,
			},
			{
				Type: openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{
					URL:    imageUrl,
					Detail: openai.ImageURLDetailLow,
				},
			},
		},
	}, streamer)
}

func extractQueryText(msg openai.ChatCompletionMessage) string {
	if msg.Content != "" {
		return msg.Content
	}
	for _, part := range msg.MultiContent {
		if part.Type == openai.ChatMessagePartTypeText && part.Text != "" {
			return part.Text
		}
	}
	return ""
}

func (h *TextService) handleLLMRequest(ctx context.Context, user models.User, tgUserMessageId int64, newMessage openai.ChatCompletionMessage, streamer *telegram_utils.TelegramStreamer) error {
	if time.Now().Unix()-user.LastInteraction > h.dialogTimeout {
		oldDialogID := user.CurrentDialogId
		go h.memoryManager.CloseDialog(context.WithoutCancel(ctx), user.Id, oldDialogID)
		user.StartNewDialog()
	}
	user.Touch()

	modelToUse := user.CurrentModel
	if !h.client.IsClientRegistered(modelToUse) {
		slog.WarnContext(ctx, "User's current model not supported, falling back to default",
			"currentModel", modelToUse,
			"defaultModel", h.defaultModel)
		modelToUse = h.defaultModel
		user.CurrentModel = h.defaultModel
	}

	if err := h.usersRepo.UpdateUser(user); err != nil {
		return err
	}

	mctx, err := h.memoryManager.BeginTurn(user.Id, user.CurrentDialogId, newMessage, tgUserMessageId)
	if err != nil {
		slog.ErrorContext(ctx, "Error beginning turn", "error", err)
		return err
	}

	queryText := extractQueryText(newMessage)
	retrieved, err := h.memoryManager.Retrieve(ctx, mctx, queryText)
	if err != nil {
		slog.ErrorContext(ctx, "Error retrieving memory", "error", err)
		return err
	}

	systemHeader := fmt.Sprintf(AssistantPrompt, time.Now().Format(time.RFC3339))
	history := h.memoryManager.AssemblePrompt(systemHeader, retrieved)

	accumulatedInputTokens := int64(0)
	accumulatedOutputTokens := int64(0)
	accumulatedResponse := ""

	tools := append(
		h.memoryService.GetMemoryTools(),
		h.reminderService.GetReminderTools()...,
	)

	for {
		stream, err := h.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
			Model:      modelToUse,
			Messages:   history,
			Tools:      tools,
			ToolChoice: "auto",
		})
		if err != nil {
			slog.ErrorContext(ctx, "Got an error while creating chat completion stream", "error", err)
			return err
		}
		defer stream.Close()

		accumulator := adapters.NewStreamAccumulator()
		for stream.Next() {
			response := stream.Current()
			accumulator.AddChunk(response)
			if err := streamer.SendChunk(response); err != nil {
				slog.ErrorContext(ctx, "Got an error while sending chunk", "error", err)
				return err
			}
		}

		if err := stream.Err(); err != nil {
			if errors.Is(err, io.EOF) {
				if err := streamer.Flush(); err != nil {
					slog.ErrorContext(ctx, "Got an error while flushing stream", "error", err)
					return err
				}
			} else {
				slog.ErrorContext(ctx, "Got an error while receiving chat completion stream", "error", err)
				return err
			}
		}
		accumulatedInputTokens += accumulator.InputTokens()
		accumulatedOutputTokens += accumulator.OutputTokens()
		accumulatedResponse = accumulator.AccumulatedResponse()

		if accumulator.HasToolCalls() {
			toolCalls := accumulator.GetToolCalls()
			slog.InfoContext(ctx, "Has tool calls", "toolCalls", toolCalls)

			if _, err := h.memoryManager.AppendModelMsg(mctx, accumulatedResponse, toolCalls, modelToUse, 0); err != nil {
				slog.ErrorContext(ctx, "Error appending model_msg with tool calls", "error", err)
				return err
			}

			history = append(history, openai.ChatCompletionMessage{
				Role:      openai.ChatMessageRoleAssistant,
				Content:   accumulatedResponse,
				ToolCalls: toolCalls,
			})

			for _, toolCall := range toolCalls {
				var result string
				var toolErr error

				switch toolCall.Function.Name {
				case "save_memory", "get_memory", "list_memories", "delete_memory", "save_fact", "forget_about":
					result, toolErr = h.memoryService.HandleToolCall(ctx, mctx, toolCall)
				case "create_reminder", "list_reminders", "cancel_reminder":
					result, toolErr = h.reminderService.HandleToolCall(user.Id, toolCall)
				default:
					toolErr = fmt.Errorf("unknown tool: %s", toolCall.Function.Name)
					result = "Unknown tool"
				}

				if _, err := h.memoryManager.AppendToolResult(mctx, toolCall.ID, toolCall.Function.Name, result); err != nil {
					slog.ErrorContext(ctx, "Error appending tool_result", "error", err)
					return err
				}

				history = append(history, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					ToolCallID: toolCall.ID,
					Content:    result,
				})
				if toolErr != nil {
					slog.ErrorContext(ctx, "Error handling tool call", "error", toolErr)
					return toolErr
				}
			}
		} else {
			if _, err := h.memoryManager.AppendModelMsg(mctx, accumulatedResponse, nil, modelToUse, 0); err != nil {
				slog.ErrorContext(ctx, "Error appending model_msg", "error", err)
				return err
			}
			break
		}
	}

	user.NumberOfInputTokens += accumulatedInputTokens
	user.NumberOfOutputTokens += accumulatedOutputTokens
	if err := h.usersRepo.UpdateUser(user); err != nil {
		slog.ErrorContext(ctx, "Error updating user token counts", "error", err)
	}

	go h.memoryManager.EndTurn(context.WithoutCancel(ctx), mctx, queryText, accumulatedResponse)

	return nil
}
