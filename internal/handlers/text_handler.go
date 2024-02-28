package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type MessagesRepo interface {
	AddMessage(message models.Interaction)
	GetCurrentDialogForUser(user models.User) []models.Interaction
}

type UsersRepo interface {
	UpdateUser(user models.User) error
}

type TextHandler struct {
	Client        *openai.Client
	MessagesRepo  MessagesRepo
	UsersRepo     UsersRepo
	DialogTimeout int64
}

func (h *TextHandler) OnTextHandler(user models.User, userText string) (string, error) {
	if time.Now().Unix()-user.LastInteraction > h.DialogTimeout {
		user.StartNewDialog()
	}
	user.Touch()
	err := h.UsersRepo.UpdateUser(user)
	if err != nil {
		return "", err
	}
	history := h.MessagesRepo.GetCurrentDialogForUser(user)
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

	response, err := h.Client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model:    openai.GPT4TurboPreview,
		Messages: messages,
	})
	if err != nil {
		return "", err
	}
	assistantResponse := response.Choices[0].Message.Content
	user.NumberOfInputTokens += int64(response.Usage.PromptTokens)
	user.NumberOfOutputTokens += int64(response.Usage.CompletionTokens)
	h.UsersRepo.UpdateUser(user)
	h.MessagesRepo.AddMessage(models.Interaction{
		UserMessage:      userText,
		AssistantMessage: assistantResponse,
		AuthorId:         user.Id,
		DialogId:         user.CurrentDialogId,
	})
	return assistantResponse, nil
}
