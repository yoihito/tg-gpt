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

func NewTextService(client LLMClient, messagesRepo MessagesRepo, usersRepo UsersRepo, memoryService *MemoryService, dialogTimeout int64, defaultModel string) *TextService {
	return &TextService{
		client:        client,
		messagesRepo:  messagesRepo,
		usersRepo:     usersRepo,
		memoryService: memoryService,
		dialogTimeout: dialogTimeout,
		defaultModel:  defaultModel,
	}
}

type TextService struct {
	client        LLMClient
	messagesRepo  MessagesRepo
	usersRepo     UsersRepo
	memoryService *MemoryService
	dialogTimeout int64
	defaultModel  string
}

type LLMClient interface {
	CreateChatCompletionStream(ctx context.Context, request openai.ChatCompletionRequest) (adapters.LLMStream, error)
	IsClientRegistered(modelId string) bool
}

type MessagesRepo interface {
	AddMessage(message models.Interaction) error
	GetCurrentDialogForUser(user models.User) ([]models.Interaction, error)
	PopLatestInteraction(user models.User) (models.Interaction, error)
}

type UsersRepo interface {
	UpdateUser(user models.User) error
}

const EOFStatus = "EOF"
const AssistantPrompt = `
You are a helpful assistant. Your name is Johhny. You are provided with a list of memories about the user. Additionally, you can add new records to your memory. IT IS VERY IMPORTANT to capture all the smallest details about the user. Today is %s. Give short concise answers.
<memories>%s</memories>
`

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

func (h *TextService) RetryInteraction(
	ctx context.Context,
	user models.User,
	tgUserMessageId int64,
	interaction models.Interaction,
	streamer *telegram_utils.TelegramStreamer,
) error {
	return h.handleLLMRequest(ctx, user, tgUserMessageId, interaction.UserMessage, streamer)
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

func (h *TextService) handleLLMRequest(ctx context.Context, user models.User, tgUserMessageId int64, newMessage openai.ChatCompletionMessage, streamer *telegram_utils.TelegramStreamer) error {
	if time.Now().Unix()-user.LastInteraction > h.dialogTimeout {
		user.StartNewDialog()
	}
	user.Touch()

	// Validate user's current model and fallback to default if not supported
	modelToUse := user.CurrentModel
	if !h.client.IsClientRegistered(modelToUse) {
		slog.WarnContext(ctx, "User's current model not supported, falling back to default",
			"currentModel", modelToUse,
			"defaultModel", h.defaultModel)
		modelToUse = h.defaultModel
		user.CurrentModel = h.defaultModel
	}

	err := h.usersRepo.UpdateUser(user)
	if err != nil {
		return err
	}

	interactionHistory, err := h.messagesRepo.GetCurrentDialogForUser(user)
	if err != nil {
		slog.ErrorContext(ctx, "Error getting current dialog for user", "error", err)
		return err
	}
	history := []openai.ChatCompletionMessage{{
		Role: openai.ChatMessageRoleSystem,
		Content: fmt.Sprintf(
			AssistantPrompt,
			time.Now().Format(time.RFC3339),
			h.memoryService.GetMemoryContext(user.Id),
		),
	}}
	for _, interaction := range interactionHistory {
		history = append(history, interaction.UserMessage, interaction.AssistantMessage)
	}
	history = append(history, newMessage)

	accumulatedInputTokens := int64(0)
	accumulatedOutputTokens := int64(0)
	accumulatedResponse := ""
	for {
		stream, err := h.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
			Model:      modelToUse,
			Messages:   history,
			Tools:      h.memoryService.GetMemoryTools(),
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
			err := streamer.SendChunk(response)
			if err != nil {
				slog.ErrorContext(ctx, "Got an error while sending chunk", "error", err)
				return err
			}
		}

		if err := stream.Err(); err != nil {
			if errors.Is(err, io.EOF) {
				err := streamer.Flush()
				if err != nil {
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
			slog.InfoContext(ctx, "Has tool calls", "toolCalls", accumulator.GetToolCalls())
			toolCalls := accumulator.GetToolCalls()
			history = append(history, openai.ChatCompletionMessage{
				Role:      openai.ChatMessageRoleAssistant,
				Content:   accumulatedResponse,
				ToolCalls: toolCalls,
			})
			for _, toolCall := range toolCalls {
				result, err := h.memoryService.HandleToolCall(user.Id, toolCall)
				history = append(history, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					ToolCallID: toolCall.ID,
					Content:    result,
				})
				if err != nil {
					slog.ErrorContext(ctx, "Error handling tool call", "error", err)
					return err
				}
			}
		} else {
			break
		}
	}

	user.NumberOfInputTokens += accumulatedInputTokens
	user.NumberOfOutputTokens += accumulatedOutputTokens
	h.usersRepo.UpdateUser(user)
	err = h.messagesRepo.AddMessage(models.Interaction{
		UserMessage: newMessage,
		AssistantMessage: openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: accumulatedResponse,
		},
		AuthorId:        user.Id,
		DialogId:        user.CurrentDialogId,
		TgUserMessageId: tgUserMessageId,
	})
	if err != nil {
		slog.ErrorContext(ctx, "Error adding message", "error", err)
		return err
	}
	return nil
}
