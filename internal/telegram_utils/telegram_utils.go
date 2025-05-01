package telegram_utils

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

const MaxTelegramMessageLength = 4096
const StreamingInterval = 200

type TelegramStreamer struct {
	messages           []*tele.Message
	currentMessage     *tele.Message
	replyTo            *tele.Message
	accumulatedMessage string
	prevLength         int
	c                  tele.Context
}

func NewTelegramStreamer(c tele.Context, replyTo *tele.Message) *TelegramStreamer {
	return &TelegramStreamer{
		c:        c,
		replyTo:  replyTo,
		messages: []*tele.Message{},
	}
}

func (t *TelegramStreamer) SendChunk(chunk openai.ChatCompletionStreamResponse) error {
	ctx := t.c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Streaming chunk", "chunk", chunk)
	if len(chunk.Choices) > 0 {
		textChunk := chunk.Choices[0].Delta.Content
		if len(t.accumulatedMessage)+len(textChunk) >= MaxTelegramMessageLength {
			if t.prevLength != len(t.accumulatedMessage) {
				err := t.Flush()
				if err != nil {
					return err
				}
			}
			t.messages = append(t.messages, t.currentMessage)
			t.currentMessage = nil
			t.accumulatedMessage = ""
			t.prevLength = 0
		}
		t.accumulatedMessage += chunk.Choices[0].Delta.Content
		if len(t.accumulatedMessage)-t.prevLength < StreamingInterval {
			return nil
		}
		t.prevLength = len(t.accumulatedMessage)
		return t.Flush()
	}
	return nil
}

func (t *TelegramStreamer) Flush() error {
	ctx := t.c.Get("requestContext").(context.Context)
	var err error
	if t.currentMessage == nil {
		t.currentMessage, err = t.c.Bot().Reply(t.replyTo, FixMarkdown(t.accumulatedMessage), &tele.SendOptions{ParseMode: tele.ModeMarkdown})
		if err != nil {
			t.currentMessage, err = t.c.Bot().Reply(t.replyTo, t.accumulatedMessage, &tele.SendOptions{ParseMode: tele.ModeDefault})
			slog.ErrorContext(ctx, "Error sending message", "error", err)
		}
	} else {
		_, err = t.c.Bot().Edit(t.currentMessage, FixMarkdown(t.accumulatedMessage), &tele.SendOptions{ParseMode: tele.ModeMarkdown})
		if err != nil {
			_, err = t.c.Bot().Edit(t.currentMessage, t.accumulatedMessage, &tele.SendOptions{ParseMode: tele.ModeDefault})
			slog.ErrorContext(ctx, "Error editing message", "error", err)
		}
	}

	if err != nil {
		slog.ErrorContext(ctx, "Error streaming", "error", err)
		noticeErr := t.c.Send("Failed to answer the message")
		if noticeErr != nil {
			slog.ErrorContext(ctx, "Error sending notice", "error", noticeErr)
		}
		return errors.Join(err, noticeErr)
	}
	return nil
}

func FixMarkdown(markdown string) string {
	tag := GetUnclosedTag(markdown)
	if tag == "" {
		return markdown
	}
	return markdown + tag
}

func GetUnclosedTag(markdown string) string {
	// order is important!
	var tags = []string{
		"```",
		"`",
		"*",
		"_",
	}
	var currentTag = ""

	markdownRunes := []rune(markdown)

	var i = 0
outer:
	for i < len(markdownRunes) {
		// skip escaped characters (only outside tags)
		if markdownRunes[i] == '\\' && currentTag == "" {
			i += 2
			continue
		}
		if currentTag != "" {
			if strings.HasPrefix(string(markdownRunes[i:]), currentTag) {
				// turn a tag off
				i += len(currentTag)
				currentTag = ""
				continue
			}
		} else {
			for _, tag := range tags {
				if strings.HasPrefix(string(markdownRunes[i:]), tag) {
					// turn a tag on
					currentTag = tag
					i += len(currentTag)
					continue outer
				}
			}
		}
		i++
	}

	return currentTag
}
