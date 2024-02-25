package handlers

import (
	"context"
	"fmt"
	"io"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type VoiceHandler struct {
	TextHandler TextHandler
	Client      *openai.Client
}

type Result struct {
	Text string
	Err  error
}

func (h *VoiceHandler) OnVoiceHandler(user models.User, voiceFileReader io.ReadCloser) (<-chan Result, func()) {
	messagesCh := make(chan Result)
	done := make(chan struct{})
	cancel := func() {
		close(done)
	}
	go func() {
		defer close(messagesCh)
		response, err := h.Client.CreateTranscription(context.Background(), openai.AudioRequest{
			Reader:   voiceFileReader,
			FilePath: "voice.ogg",
			Model:    openai.Whisper1,
		})
		if err != nil {
			messagesCh <- Result{Err: err}
			return
		}
		messagesCh <- Result{Text: fmt.Sprintf("Transcription: _%s_", response.Text)}
		botResponse, err := h.TextHandler.OnTextHandler(user, response.Text)
		if err != nil {
			messagesCh <- Result{Err: err}
			return
		}
		messagesCh <- Result{Text: botResponse}
		return
	}()
	return messagesCh, cancel
}
