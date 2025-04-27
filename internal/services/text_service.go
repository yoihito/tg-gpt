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
	AddMessage(message models.Interaction)
	GetCurrentDialogForUser(user models.User) []models.Interaction
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

func (h *TextService) RetryInteraction(ctx context.Context, user models.User, tgUserMessageId int64, interaction models.Interaction) <-chan Result {
	return h.handleLLMRequest(ctx, user, tgUserMessageId, interaction.UserMessage)
}

func (h *TextService) OnStreamableTextHandler(ctx context.Context, user models.User, tgUserMessageId int64, userText string) <-chan Result {
	return h.handleLLMRequest(ctx, user, tgUserMessageId, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: userText,
	})
}

func (h *TextService) OnStreamableVisionHandler(ctx context.Context, user models.User, tgUserMessageId int64, userText string, imageUrl string) <-chan Result {
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
	})
}

func (h *TextService) handleLLMRequest(ctx context.Context, user models.User, tgUserMessageId int64, newMessage openai.ChatCompletionMessage) <-chan Result {
	resultsCh := make(chan Result)
	go func() {
		defer close(resultsCh)
		if time.Now().Unix()-user.LastInteraction > h.dialogTimeout {
			user.StartNewDialog()
		}
		user.Touch()
		err := h.usersRepo.UpdateUser(user)
		if err != nil {
			resultsCh <- Result{Err: err}
			return
		}

		interactionHistory := h.messagesRepo.GetCurrentDialogForUser(user)
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
			resultsCh <- Result{Err: err}
			return
		}
		defer stream.Close()

	streamLoop:
		for {
			select {
			case <-ctx.Done():
				return
			default:
				response, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					resultsCh <- Result{Status: EOFStatus}
					break streamLoop
				}
				if err != nil {
					slog.ErrorContext(ctx, "Got an error while receiving chat completion stream", "error", err)
					resultsCh <- Result{Err: err}
					return
				}
				resultsCh <- Result{ChunkResponse: response}
			}
		}

		outputTokens, err := stream.OutputTokens()
		if err != nil {
			slog.ErrorContext(ctx, "Got an error while getting output tokens", "error", err)
			resultsCh <- Result{Err: err}
			return
		}
		user.NumberOfInputTokens += stream.InputTokens()
		user.NumberOfOutputTokens += int64(outputTokens)
		h.usersRepo.UpdateUser(user)
		h.messagesRepo.AddMessage(models.Interaction{
			UserMessage: newMessage,
			AssistantMessage: openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: stream.AccumulatedResponse(),
			},
			AuthorId:        user.Id,
			DialogId:        user.CurrentDialogId,
			TgUserMessageId: tgUserMessageId,
		})
	}()
	return resultsCh
}
