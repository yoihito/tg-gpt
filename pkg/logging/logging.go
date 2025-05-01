package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

type ContextHandler struct {
	slog.Handler
}

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if requestID, ok := ctx.Value("request_id").(string); ok {
		r.AddAttrs(slog.String("request_id", requestID))
	}
	if userID, ok := ctx.Value("user_id").(int64); ok {
		r.AddAttrs(slog.String("user_id", fmt.Sprintf("%d", userID)))
	}
	if tgUserID, ok := ctx.Value("tg_user_id").(int64); ok {
		r.AddAttrs(slog.String("tg_user_id", fmt.Sprintf("%d", tgUserID)))
	}
	return h.Handler.Handle(ctx, r)
}

func SetupLogger(ctx context.Context) error {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") != "" {
		err := logLevel.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
		if err != nil {
			slog.ErrorContext(ctx, "Error parsing log level", "error", err)
			return err
		}
	}
	logger := slog.New(&ContextHandler{slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &logLevel, AddSource: true})})
	slog.SetDefault(logger)
	return nil
}
