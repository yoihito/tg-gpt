package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"
)

type Extractor struct {
	client *openai.Client
	model  string
}

func NewExtractor(client *openai.Client, model string) *Extractor {
	return &Extractor{client: client, model: model}
}

type CandidateType string

const (
	CandidatePreference CandidateType = "preference"
	CandidateFact       CandidateType = "fact"
)

type Candidate struct {
	Type       CandidateType `json:"type"`
	Key        string        `json:"key,omitempty"`     // preferences only
	Subject    string        `json:"subject,omitempty"` // facts only
	Value      string        `json:"value,omitempty"`   // preferences only
	Content    string        `json:"content,omitempty"` // facts only
	Confidence float64       `json:"confidence"`
}

type ExtractInput struct {
	UserMessage      string
	AssistantMessage string
	RecentContext    string // optional compact summary of the last few turns
}

const extractorSystemPrompt = `You analyze a single user/assistant exchange and extract durable, memorable information about the user.

Output STRICT JSON exactly matching:
{"candidates": [
  {"type": "preference", "key": "<snake_case>", "value": "<short value>", "confidence": 0.0-1.0},
  {"type": "fact", "subject": "<snake_case e.g. self, wife_anna, company_acme>", "content": "<one sentence>", "confidence": 0.0-1.0}
]}

RULES:
- Preferences are STABLE user preferences (format, locale, language, tone, timezone, communication style).
- Facts are DURABLE facts about the user, their life, work, relationships, possessions.
- Do NOT extract:
  - One-time intents or current requests ("send a message", "remind me at 3pm").
  - Questions the user asked.
  - Information about anyone other than the user (unless it is about the user's relationship to that entity).
  - Generic chitchat or assistant statements.
- Be conservative. When in doubt, output an empty list.
- Confidence: 0.95+ for explicitly stated; 0.7-0.9 for clearly implied; below 0.7 means do not include.

EXAMPLES:
User: "I live in Berlin."
Output: {"candidates":[{"type":"fact","subject":"self","content":"Lives in Berlin.","confidence":0.95}]}

User: "Always reply in French please."
Output: {"candidates":[{"type":"preference","key":"response_language","value":"French","confidence":0.95}]}

User: "What's the weather?"
Output: {"candidates":[]}

User: "My wife Anna is a doctor."
Output: {"candidates":[
  {"type":"fact","subject":"wife_anna","content":"Wife is named Anna.","confidence":0.95},
  {"type":"fact","subject":"wife_anna","content":"Wife Anna is a doctor.","confidence":0.95}
]}`

// Extract calls the configured cheap model to propose memory candidates from a turn.
// Returns nil on any non-fatal error (extraction must not break the user-facing flow).
func (e *Extractor) Extract(ctx context.Context, in ExtractInput) ([]Candidate, error) {
	userPart := strings.Builder{}
	if in.RecentContext != "" {
		userPart.WriteString("Recent context:\n")
		userPart.WriteString(in.RecentContext)
		userPart.WriteString("\n\n")
	}
	userPart.WriteString("User message:\n")
	userPart.WriteString(in.UserMessage)
	userPart.WriteString("\n\nAssistant response:\n")
	userPart.WriteString(in.AssistantMessage)

	resp, err := e.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: e.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: extractorSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPart.String()},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("extractor completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("extractor: empty choices")
	}
	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	if raw == "" {
		return nil, nil
	}

	var out struct {
		Candidates []Candidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("extractor parse: %w (raw=%q)", err, raw)
	}
	return out.Candidates, nil
}
