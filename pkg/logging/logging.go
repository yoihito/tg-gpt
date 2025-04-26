package logging

import (
	"log/slog"
	"os"
)

func SetupLogger() error {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") != "" {
		err := logLevel.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
		if err != nil {
			slog.Error("Error parsing log level", "error", err)
			return err
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &logLevel}))
	slog.SetDefault(logger)
	return nil
}
