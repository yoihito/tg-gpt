package models

type Interaction struct {
	UserMessage      string
	AssistantMessage string
	AuthorId         int64
	DialogId         int64
	TgUserMessageId  int64
}
