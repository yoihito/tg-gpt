package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

const openaiSummarizerTimeout = 90 * time.Second

type Summarizer struct {
	client *openai.Client
	model  string
}

func NewSummarizer(client *openai.Client, model string) *Summarizer {
	return &Summarizer{client: client, model: model}
}

const summarizerSystemPrompt = `You summarize a single dialog between a user and an assistant.

Write a 2-3 sentence summary capturing:
- What the user wanted, asked about, or worked on
- The outcome or current state of the conversation
- Any notable decisions, facts, or follow-ups

Be concise, factual, and write in past tense. Output the summary as plain text only — no preamble, no quotes, no JSON.`

// Summarize produces a short past-tense summary of a dialog. Returns empty string
// when there isn't enough material to summarize.
func (s *Summarizer) Summarize(ctx context.Context, events []models.TraceEvent) (string, error) {
	transcript := renderTranscript(events)
	if strings.TrimSpace(transcript) == "" {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(ctx, openaiSummarizerTimeout)
	defer cancel()

	slog.InfoContext(ctx, "OpenAI summarizer: starting",
		"model", s.model,
		"events", len(events),
		"transcript_len", len(transcript),
		"timeout", openaiSummarizerTimeout.String(),
	)
	resp, err := s.client.Responses.New(ctx, responses.ResponseNewParams{
		Model:        shared.ResponsesModel(s.model),
		Instructions: openai.String(summarizerSystemPrompt),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String("Dialog:\n\n" + transcript),
		},
	})
	if err != nil {
		slog.ErrorContext(ctx, "OpenAI summarizer: failed", "model", s.model, "error", err)
		return "", fmt.Errorf("summarizer completion: %w", err)
	}
	summary := strings.TrimSpace(resp.OutputText())
	slog.InfoContext(ctx, "OpenAI summarizer: completed",
		"model", s.model,
		"summary_len", len(summary),
	)
	return summary, nil
}

func renderTranscript(events []models.TraceEvent) string {
	var b strings.Builder
	for _, e := range events {
		switch e.EventType {
		case models.EventTypeUserMsg:
			var p models.UserMsgPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			text := p.Content
			if text == "" {
				for _, part := range p.MultiContent {
					if part.Type == llm.ContentPartText {
						text = part.Text
						break
					}
				}
			}
			if text == "" {
				continue
			}
			fmt.Fprintf(&b, "User: %s\n", text)
		case models.EventTypeModelMsg:
			var p models.ModelMsgPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			if p.Content == "" {
				continue
			}
			fmt.Fprintf(&b, "Assistant: %s\n", p.Content)
		case models.EventTypeToolResult:
			var p models.ToolResultPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			fmt.Fprintf(&b, "Tool %s: %s\n", p.Name, p.Result)
		}
	}
	return b.String()
}
