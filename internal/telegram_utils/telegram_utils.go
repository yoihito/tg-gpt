package telegram_utils

import (
	"context"
	"log/slog"
	"strings"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

const MaxTelegramMessageLength = 4096
const StreamingInterval = 200

type TelegramStreamer struct {
	currentMessage     *tele.Message
	replyTo            *tele.Message
	accumulatedMessage string
	prevLength         int
	bot                *tele.Bot
	ctx                context.Context
	delivery           *chatDelivery
}

func NewTelegramStreamer(c tele.Context, replyTo *tele.Message) *TelegramStreamer {
	return &TelegramStreamer{
		bot:      c.Bot(),
		ctx:      c.Get("requestContext").(context.Context),
		delivery: deliveryFor(c.Bot(), replyTo.Chat.Recipient()),
		replyTo:  replyTo,
	}
}

// SetContext binds delivery waits to the dispatched turn so /cancel interrupts them.
func (t *TelegramStreamer) SetContext(ctx context.Context) { t.ctx = ctx }

func (t *TelegramStreamer) HasOutput() bool {
	return t.currentMessage != nil || strings.TrimSpace(t.accumulatedMessage) != ""
}

func (t *TelegramStreamer) SendStatus(text string) error {
	if t.HasOutput() || strings.TrimSpace(text) == "" {
		return nil
	}
	msg, _, err := t.delivery.deliver(t.ctx, true, func() (*tele.Message, error) {
		return t.bot.Reply(t.replyTo, text, &tele.SendOptions{ParseMode: tele.ModeDefault, ThreadID: t.replyTo.ThreadID})
	})
	if err != nil {
		slog.ErrorContext(t.ctx, "Error sending status message", "error", err, "message", text)
		return err
	}
	t.currentMessage = msg
	return nil
}

func (t *TelegramStreamer) SendEvent(event llm.StreamEvent) error {
	slog.DebugContext(t.ctx, "Streaming event", "event", event)
	if event.TextDelta == "" {
		return nil
	}
	t.accumulatedMessage += event.TextDelta
	for {
		chunk, rest := splitMessage(t.accumulatedMessage)
		t.accumulatedMessage = chunk
		if rest == "" {
			break
		}
		if err := t.Flush(); err != nil {
			return err
		}
		t.currentMessage = nil
		t.accumulatedMessage = rest
		t.prevLength = 0
	}

	if len(t.accumulatedMessage)-t.prevLength < StreamingInterval {
		return nil
	}
	return t.flush(false)
}

func (t *TelegramStreamer) Flush() error { return t.flush(true) }

func (t *TelegramStreamer) flush(wait bool) error {
	if strings.TrimSpace(t.accumulatedMessage) == "" || t.prevLength == len(t.accumulatedMessage) {
		return nil
	}
	// Intermediate updates hold back a Markdown link that's still being streamed
	// in: unlike bold/italic/code, an in-progress "[text](url" can't be closed
	// without the rest of the URL, so sending it as-is would trip Telegram's
	// entity parser. The final flush (wait==true) always sends everything,
	// relying on withMarkdownFallback's plain-text fallback for anything that's
	// genuinely malformed rather than just unfinished.
	textToSend := t.accumulatedMessage
	if !wait {
		textToSend = trimIncompleteLink(textToSend)
		if strings.TrimSpace(textToSend) == "" {
			return nil
		}
	}
	msg, delivered, err := t.delivery.deliver(t.ctx, wait, func() (*tele.Message, error) {
		return withMarkdownFallback(textToSend, func(text string, mode tele.ParseMode) (*tele.Message, error) {
			options := &tele.SendOptions{ParseMode: mode, ThreadID: t.replyTo.ThreadID}
			if t.currentMessage == nil {
				return t.bot.Reply(t.replyTo, text, options)
			}
			return t.bot.Edit(t.currentMessage, text, options)
		})
	})
	if err != nil {
		slog.ErrorContext(t.ctx, "Error streaming", "error", err)
		return err
	}
	if delivered {
		if t.currentMessage == nil {
			t.currentMessage = msg
		}
		t.prevLength = len(textToSend)
	}
	return nil
}

// SendChunked sends text to recipient as one or more Telegram messages, splitting at
// MaxTelegramMessageLength and falling back to plain text if a chunk's Markdown fails to
// parse. This is the one-shot sibling of TelegramStreamer.Flush — both implement the same
// "try Markdown, fall back to plain, respect the length limit" contract for the same API,
// so every caller that needs to push text to a chat outside a live stream shares this
// instead of writing its own copy.
func SendChunked(ctx context.Context, bot *tele.Bot, recipient tele.Recipient, text string) error {
	delivery := deliveryFor(bot, recipient.Recipient())
	for text != "" {
		chunk, rest := splitMessage(text)
		_, _, err := delivery.deliver(ctx, true, func() (*tele.Message, error) {
			return withMarkdownFallback(chunk, func(text string, mode tele.ParseMode) (*tele.Message, error) {
				return bot.Send(recipient, text, &tele.SendOptions{ParseMode: mode})
			})
		})
		if err != nil {
			return err
		}
		text = rest
	}
	return nil
}

// trimIncompleteLink returns the longest prefix of text that doesn't end in the
// middle of a Markdown link ([text](url)). A link still being streamed in can't
// be closed the way an unclosed bold/italic/code span can, since the rest of the
// URL isn't known yet.
func trimIncompleteLink(text string) string {
	const (
		normal = iota
		afterOpenBracket
		afterCloseBracket
		afterOpenParen
	)
	runes := []rune(text)
	state := normal
	linkStart := -1
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' {
			i++ // skip escaped character without affecting link state
			continue
		}
		r := runes[i]
		if state == afterCloseBracket && r != '(' {
			state = normal
			linkStart = -1
		}
		switch {
		case r == '[' && state == normal:
			state = afterOpenBracket
			linkStart = i
		case r == ']' && state == afterOpenBracket:
			state = afterCloseBracket
		case r == '(' && state == afterCloseBracket:
			state = afterOpenParen
		case r == ')' && state == afterOpenParen:
			state = normal
			linkStart = -1
		}
	}
	if state != normal && linkStart >= 0 {
		return string(runes[:linkStart])
	}
	return text
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
