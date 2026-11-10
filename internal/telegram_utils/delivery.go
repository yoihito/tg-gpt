package telegram_utils

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	tele "gopkg.in/telebot.v3"
)

const telegramUpdateInterval = 2 * time.Second

type deliveryKey struct {
	bot       *tele.Bot
	recipient string
}

type chatDelivery struct {
	gate     chan struct{}
	nextCall time.Time
}

var chatDeliveries sync.Map

func deliveryFor(bot *tele.Bot, recipient string) *chatDelivery {
	key := deliveryKey{bot, recipient}
	if value, ok := chatDeliveries.Load(key); ok {
		return value.(*chatDelivery)
	}
	value, _ := chatDeliveries.LoadOrStore(key, &chatDelivery{gate: make(chan struct{}, 1)})
	return value.(*chatDelivery)
}

// deliver coalesces intermediate updates during cooldown. Final updates wait and
// retry the same operation, including Telegram's server-provided flood timeout.
func (d *chatDelivery) deliver(ctx context.Context, wait bool, operation func() (*tele.Message, error)) (*tele.Message, bool, error) {
	acquired, err := d.acquire(ctx, wait)
	if err != nil || !acquired {
		return nil, false, err
	}
	defer func() { <-d.gate }()

	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if delay := time.Until(d.nextCall); delay > 0 {
			if !wait {
				return nil, false, nil
			}
			if err := waitForCooldown(ctx, delay); err != nil {
				return nil, false, err
			}
		}
		msg, err := operation()
		d.nextCall = time.Now().Add(telegramUpdateInterval)
		var flood tele.FloodError
		if !errors.As(err, &flood) {
			if errors.Is(err, tele.ErrMessageNotModified) || errors.Is(err, tele.ErrSameMessageContent) {
				err = nil
			}
			return msg, err == nil, err
		}
		delay := max(time.Duration(flood.RetryAfter)*time.Second, telegramUpdateInterval)
		d.nextCall = time.Now().Add(delay)
		slog.WarnContext(ctx, "Telegram flood cooldown", "retry_after", flood.RetryAfter)
		if !wait {
			return nil, false, nil
		}
	}
}

// Intermediate updates skip a busy chat; required updates acquire it cancellably.
func (d *chatDelivery) acquire(ctx context.Context, wait bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !wait {
		select {
		case d.gate <- struct{}{}:
			return true, nil
		default:
			return false, nil
		}
	}
	select {
	case d.gate <- struct{}{}:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func waitForCooldown(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Fall back only for formatting errors, leaving flood errors to the retry loop.
func withMarkdownFallback(text string, send func(string, tele.ParseMode) (*tele.Message, error)) (*tele.Message, error) {
	msg, err := send(FixMarkdown(text), tele.ModeMarkdown)
	if isMarkdownError(err) {
		return send(text, tele.ModeDefault)
	}
	return msg, err
}

// Telebot only wraps recognized Telegram API errors in *tele.Error (see its
// errors.go lookup table); "can't parse entities" isn't in that table, so it
// comes back as a plain fmt.Errorf. Match on the message text instead of the
// error type so the Markdown fallback actually triggers for it.
func isMarkdownError(err error) bool {
	if err == nil {
		return false
	}
	var flood tele.FloodError
	if errors.As(err, &flood) {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "parse entities")
}

// splitMessage counts UTF-16 units conservatively, keeping Unicode characters
// intact and leaving space for FixMarkdown to close an unfinished formatting tag.
func splitMessage(text string) (string, string) {
	units := 0
	for offset, char := range text {
		size := 1
		if char > 0xffff {
			size = 2
		}
		if units+size > MaxTelegramMessageLength-6 {
			return text[:offset], text[offset:]
		}
		units += size
	}
	return text, ""
}
