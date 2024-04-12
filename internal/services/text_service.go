package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/adapters"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type TextServiceFactory struct {
	ClientFactory *LLMClientFactory
	MessagesRepo  MessagesRepo
	UsersRepo     UsersRepo
	DialogTimeout int64
}

func (f *TextServiceFactory) NewTextService(user models.User, tgUserMessageId int64) (*TextService, error) {
	client, err := f.ClientFactory.GetClient(user.CurrentModel)
	if err != nil {
		return nil, err
	}

	return &TextService{
		client:          client,
		messagesRepo:    f.MessagesRepo,
		usersRepo:       f.UsersRepo,
		dialogTimeout:   f.DialogTimeout,
		user:            user,
		tgUserMessageId: tgUserMessageId,
	}, nil
}

type TextService struct {
	client          LLMClient
	messagesRepo    MessagesRepo
	usersRepo       UsersRepo
	dialogTimeout   int64
	user            models.User
	tgUserMessageId int64
}

type LLMClient interface {
	CreateChatCompletionStream(ctx context.Context, request openai.ChatCompletionRequest) (adapters.LLMStream, error)
}

type MessagesRepo interface {
	AddMessage(message models.Interaction)
	GetCurrentDialogForUser(user models.User) []models.Interaction
}

type UsersRepo interface {
	UpdateUser(user models.User) error
}

const EOF_STATUS = "EOF"

type Result struct {
	Status    string
	TextChunk string
	Err       error
}

type MessagePartType string

const (
	MessagePartTypeText  MessagePartType = "text"
	MessagePartTypeImage MessagePartType = "image"
)

func (h *TextService) OnStreamableTextHandler(ctx context.Context, userText string) <-chan Result {
	return h.handleLLMRequest(ctx, models.Message{
		Content: userText,
	})
}

func (h *TextService) OnStreamableVisionHandler(ctx context.Context, userText string, imageUrl string) <-chan Result {
	return h.handleLLMRequest(ctx, models.Message{
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

func (h *TextService) handleLLMRequest(ctx context.Context, newMessage models.Message) <-chan Result {
	resultsCh := make(chan Result)
	go func() {
		defer close(resultsCh)
		if time.Now().Unix()-h.user.LastInteraction > h.dialogTimeout {
			h.user.StartNewDialog()
		}
		h.user.Touch()
		err := h.usersRepo.UpdateUser(h.user)
		if err != nil {
			resultsCh <- Result{Err: err}
			return
		}
		history := h.messagesRepo.GetCurrentDialogForUser(h.user)
		messages := []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: fmt.Sprintf("You are a helpful assistant. Your name is Johhny. Today is %s. Give short concise answers.", time.Now().Format(time.RFC3339)),
			},
		}
		for _, interaction := range history {
			messages = append(messages, interaction.UserChatCompletion(), interaction.AssistantChatCompletion())
		}
		messages = append(messages, openai.ChatCompletionMessage{
			Role:         openai.ChatMessageRoleUser,
			Content:      newMessage.Content,
			MultiContent: newMessage.MultiContent,
		})

		stream, err := h.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
			Messages: messages,
		})
		if err != nil {
			log.Println(err)
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
					resultsCh <- Result{Status: EOF_STATUS}
					break streamLoop
				}
				if err != nil {
					log.Println(err)
					resultsCh <- Result{Err: err}
					return
				}
				resultsCh <- Result{TextChunk: response.Choices[0].Delta.Content}
			}
		}

		outputTokens, err := stream.OutputTokens()
		if err != nil {
			log.Println(err)
			resultsCh <- Result{Err: err}
			return
		}
		h.user.NumberOfInputTokens += stream.InputTokens()
		h.user.NumberOfOutputTokens += int64(outputTokens)
		h.usersRepo.UpdateUser(h.user)
		h.messagesRepo.AddMessage(models.Interaction{
			UserMessage: newMessage,
			AssistantMessage: models.Message{
				Content: stream.AccumulatedResponse(),
			},
			AuthorId:        h.user.Id,
			DialogId:        h.user.CurrentDialogId,
			TgUserMessageId: h.tgUserMessageId,
		})
	}()
	return resultsCh
}
