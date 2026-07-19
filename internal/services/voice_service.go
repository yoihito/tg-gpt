package services

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/openai/openai-go/v3"
)

const openaiTranscriptionTimeout = 2 * time.Minute

type VoiceService struct {
	Client *openai.Client
}

func (h *VoiceService) OnVoiceHandler(ctx context.Context, voiceFileReader io.ReadCloser) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, openaiTranscriptionTimeout)
	defer cancel()

	slog.InfoContext(ctx, "OpenAI transcription: starting",
		"model", openai.AudioModelWhisper1,
		"timeout", openaiTranscriptionTimeout.String(),
	)
	response, err := h.Client.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  openai.File(voiceFileReader, "voice.ogg", "audio/ogg"),
		Model: openai.AudioModelWhisper1,
	})
	if err != nil {
		slog.ErrorContext(ctx, "OpenAI transcription: failed", "error", err)
		return "", err
	}
	slog.InfoContext(ctx, "OpenAI transcription: completed", "text_len", len(response.Text))
	return response.Text, nil
}
