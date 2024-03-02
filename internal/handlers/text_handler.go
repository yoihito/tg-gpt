package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type TextHandlerFactory struct {
	Client        *openai.Client
	MessagesRepo  MessagesRepo
	UsersRepo     UsersRepo
	DialogTimeout int64
}

func (f *TextHandlerFactory) NewTextHandler(user models.User, tgUserMessageId int64) *TextHandler {
	return &TextHandler{
		client:          f.Client,
		messagesRepo:    f.MessagesRepo,
		usersRepo:       f.UsersRepo,
		dialogTimeout:   f.DialogTimeout,
		user:            user,
		tgUserMessageId: tgUserMessageId,
	}
}

type TextHandler struct {
	client          *openai.Client
	messagesRepo    MessagesRepo
	usersRepo       UsersRepo
	dialogTimeout   int64
	user            models.User
	tgUserMessageId int64
}

type MessagesRepo interface {
	AddMessage(message models.Interaction)
	GetCurrentDialogForUser(user models.User) []models.Interaction
}

type UsersRepo interface {
	UpdateUser(user models.User) error
}

func (h *TextHandler) OnTextHandler(userText string) (string, error) {
	if time.Now().Unix()-h.user.LastInteraction > h.dialogTimeout {
		h.user.StartNewDialog()
	}
	h.user.Touch()
	err := h.usersRepo.UpdateUser(h.user)
	if err != nil {
		return "", err
	}
	history := h.messagesRepo.GetCurrentDialogForUser(h.user)
	messages := []openai.ChatCompletionMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: fmt.Sprintf("You are a helpful assistant. Your name is Johhny. Today is %s. Give short concise answers.", time.Now().Format(time.RFC3339)),
		},
	}
	for _, message := range history {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    "user",
			Content: message.UserMessage,
		}, openai.ChatCompletionMessage{
			Role:    "assistant",
			Content: message.AssistantMessage,
		})
	}
	messages = append(messages, openai.ChatCompletionMessage{
		Role:    "user",
		Content: userText,
	})

	response, err := h.client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model:    openai.GPT4TurboPreview,
		Messages: messages,
	})
	if err != nil {
		return "", err
	}
	assistantResponse := response.Choices[0].Message.Content
	h.user.NumberOfInputTokens += int64(response.Usage.PromptTokens)
	h.user.NumberOfOutputTokens += int64(response.Usage.CompletionTokens)
	h.usersRepo.UpdateUser(h.user)
	h.messagesRepo.AddMessage(models.Interaction{
		UserMessage:      userText,
		AssistantMessage: assistantResponse,
		AuthorId:         h.user.Id,
		DialogId:         h.user.CurrentDialogId,
		TgUserMessageId:  h.tgUserMessageId,
	})
	return assistantResponse, nil
}

const EOF_STATUS = "EOF"

type Result struct {
	Status    string
	TextChunk string
	Err       error
}

func (h *TextHandler) OnStreamableTextHandler(ctx context.Context, userText string) <-chan Result {
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
		for _, message := range history {
			messages = append(messages, openai.ChatCompletionMessage{
				Role:    "user",
				Content: message.UserMessage,
			}, openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: message.AssistantMessage,
			})
		}
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    "user",
			Content: userText,
		})

		stream, err := h.client.CreateChatCompletionStream(ctx, openai.ChatCompletionRequest{
			Model:    openai.GPT4TurboPreview,
			Messages: messages,
		})
		if err != nil {
			resultsCh <- Result{Err: err}
			return
		}
		defer stream.Close()
		assistantResponse := ""
		for {
			response, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				resultsCh <- Result{Status: EOF_STATUS}
				break
			}
			if err != nil {
				resultsCh <- Result{Err: err}
				return
			}
			assistantResponse = assistantResponse + response.Choices[0].Delta.Content
			resultsCh <- Result{TextChunk: response.Choices[0].Delta.Content}
		}

		h.usersRepo.UpdateUser(h.user)
		h.messagesRepo.AddMessage(models.Interaction{
			UserMessage:      userText,
			AssistantMessage: assistantResponse,
			AuthorId:         h.user.Id,
			DialogId:         h.user.CurrentDialogId,
			TgUserMessageId:  h.tgUserMessageId,
		})
	}()
	return resultsCh
}
