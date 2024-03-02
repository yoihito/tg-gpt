package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/pkoukk/tiktoken-go"
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

func (h *TextHandler) OnTextHandler(ctx context.Context, userText string) (string, error) {
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

	response, err := h.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
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
			log.Println(err)
			resultsCh <- Result{Err: err}
			return
		}
		defer stream.Close()

		assistantResponse := ""
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
				assistantResponse = assistantResponse + response.Choices[0].Delta.Content
				resultsCh <- Result{TextChunk: response.Choices[0].Delta.Content}
			}
		}

		encoder, err := tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			log.Println(err)
			resultsCh <- Result{Err: err}
			return
		}
		h.user.NumberOfInputTokens += int64(NumTokensFromMessages(messages, openai.GPT4TurboPreview))
		h.user.NumberOfOutputTokens += int64(len(encoder.Encode(assistantResponse, nil, nil)))
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

// OpenAI Cookbook: https://github.com/openai/openai-cookbook/blob/main/examples/How_to_count_tokens_with_tiktoken.ipynb
func NumTokensFromMessages(messages []openai.ChatCompletionMessage, model string) (numTokens int) {
	tkm, err := tiktoken.EncodingForModel(model)
	if err != nil {
		err = fmt.Errorf("encoding for model: %v", err)
		log.Println(err)
		return
	}

	var tokensPerMessage, tokensPerName int
	switch model {
	case "gpt-3.5-turbo-0613",
		"gpt-3.5-turbo-16k-0613",
		"gpt-4-0314",
		"gpt-4-32k-0314",
		"gpt-4-0613",
		"gpt-4-32k-0613":
		tokensPerMessage = 3
		tokensPerName = 1
	case "gpt-3.5-turbo-0301":
		tokensPerMessage = 4 // every message follows <|start|>{role/name}\n{content}<|end|>\n
		tokensPerName = -1   // if there's a name, the role is omitted
	default:
		if strings.Contains(model, "gpt-3.5-turbo") {
			log.Println("warning: gpt-3.5-turbo may update over time. Returning num tokens assuming gpt-3.5-turbo-0613.")
			return NumTokensFromMessages(messages, "gpt-3.5-turbo-0613")
		} else if strings.Contains(model, "gpt-4") {
			log.Println("warning: gpt-4 may update over time. Returning num tokens assuming gpt-4-0613.")
			return NumTokensFromMessages(messages, "gpt-4-0613")
		} else {
			err = fmt.Errorf("num_tokens_from_messages() is not implemented for model %s. See https://github.com/openai/openai-python/blob/main/chatml.md for information on how messages are converted to tokens.", model)
			log.Println(err)
			return
		}
	}

	for _, message := range messages {
		numTokens += tokensPerMessage
		numTokens += len(tkm.Encode(message.Content, nil, nil))
		numTokens += len(tkm.Encode(message.Role, nil, nil))
		numTokens += len(tkm.Encode(message.Name, nil, nil))
		if message.Name != "" {
			numTokens += tokensPerName
		}
	}
	numTokens += 3 // every reply is primed with <|start|>assistant<|message|>
	return numTokens
}
