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

func NewTextService(client LLMClient, messagesRepo MessagesRepo, usersRepo UsersRepo, dialogTimeout int64) *TextService {
	return &TextService{
		client:        client,
		messagesRepo:  messagesRepo,
		usersRepo:     usersRepo,
		dialogTimeout: dialogTimeout,
	}
}

type TextService struct {
	client        LLMClient
	messagesRepo  MessagesRepo
	usersRepo     UsersRepo
	dialogTimeout int64
}

type LLMClient interface {
	CreateChatCompletionStream(ctx context.Context, request openai.ChatCompletionRequest) (adapters.LLMStream, error)
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
		Role:    openai.ChatMessageRoleSystem,
		Content: fmt.Sprintf("You are a helpful assistant. Your name is Johhny. Today is %s. Give short concise answers.", time.Now().Format(time.RFC3339)),
	}}
	for _, interaction := range interactionHistory {
		history = append(history, interaction.UserMessage, interaction.AssistantMessage)
	}
	history = append(history, newMessage)

	stream, err := h.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
		Model:    user.CurrentModel,
		Messages: history,
	})
	if err != nil {
		slog.ErrorContext(ctx, "Got an error while creating chat completion stream", "error", err)
		return err
	}
	defer stream.Close()

	accumulator := adapters.StreamAccumulator{}
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

	user.NumberOfInputTokens += accumulator.InputTokens()
	user.NumberOfOutputTokens += accumulator.OutputTokens()
	h.usersRepo.UpdateUser(user)
	err = h.messagesRepo.AddMessage(models.Interaction{
		UserMessage: newMessage,
		AssistantMessage: openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: accumulator.AccumulatedResponse(),
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
