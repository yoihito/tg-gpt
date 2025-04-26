package middleware

import (
	"encoding/json"
	"log/slog"

	tele "gopkg.in/telebot.v3"
)

func Logger() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			data, _ := json.MarshalIndent(c.Update(), "", "  ")
			slog.Info("User message received", "user_id", c.Sender().ID, "update", string(data))
			return next(c)
		}
	}
}
