package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

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

	resp, err := s.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: summarizerSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: "Dialog:\n\n" + transcript},
		},
	})
	if err != nil {
		return "", fmt.Errorf("summarizer completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("summarizer: empty choices")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
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
