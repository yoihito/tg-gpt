package models

import "github.com/sashabaranov/go-openai"

type Interaction struct {
	UserMessage          openai.ChatCompletionMessage
	AssistantMessage     openai.ChatCompletionMessage
	AuthorId             int64
	DialogId             int64
	TgUserMessageId      int64
	TgAssistantMessageId int64
}
