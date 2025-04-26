package services

import (
	"context"
	"io"

	"github.com/sashabaranov/go-openai"
)

type VoiceService struct {
	Client *openai.Client
}

func (h *VoiceService) OnVoiceHandler(ctx context.Context, voiceFileReader io.ReadCloser) (string, error) {
	response, err := h.Client.CreateTranscription(ctx, openai.AudioRequest{
		Reader:   voiceFileReader,
		FilePath: "voice.ogg",
		Model:    openai.Whisper1,
	})
	if err != nil {
		return "", err
	}
	return response.Text, nil
}
