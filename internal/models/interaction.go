package models

import "github.com/sashabaranov/go-openai"

type Interaction struct {
	UserMessage      Message
	AssistantMessage Message
	AuthorId         int64
	DialogId         int64
	TgUserMessageId  int64
}

func (i *Interaction) UserChatCompletion() openai.ChatCompletionMessage {
	if len(i.UserMessage.MultiContent) > 0 {
		return openai.ChatCompletionMessage{
			Role:         openai.ChatMessageRoleUser,
			MultiContent: i.UserMessage.MultiContent,
		}
	} else {
		return openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: i.UserMessage.Content,
		}
	}
}

func (i *Interaction) AssistantChatCompletion() openai.ChatCompletionMessage {
	if len(i.AssistantMessage.MultiContent) > 0 {
		return openai.ChatCompletionMessage{
			Role:         openai.ChatMessageRoleAssistant,
			MultiContent: i.AssistantMessage.MultiContent,
		}
	} else {
		return openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleAssistant,
			Content: i.AssistantMessage.Content,
		}
	}
}

type Message struct {
	Content      string
	MultiContent []openai.ChatMessagePart
}
