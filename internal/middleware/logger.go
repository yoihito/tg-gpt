package middleware

import (
	"context"
	"encoding/json"
	"log/slog"

	tele "gopkg.in/telebot.v3"
)

func Logger() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			ctx := c.Get("requestContext").(context.Context)

			data, err := json.Marshal(c.Update())
			if err != nil {
				slog.ErrorContext(ctx, "Error marshalling update", "error", err)
				return next(c)
			}

			slog.InfoContext(ctx, "User message received", "update", string(data))
			return next(c)
		}
	}
}
