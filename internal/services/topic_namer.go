package services

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const topicNamerTimeout = 20 * time.Second

// telegramTopicNameLimit is Telegram's max length for a forum topic name.
const telegramTopicNameLimit = 128

const topicNamerSystemPrompt = `Generate a short title for a chat topic, based on its first exchange (the user's
opening message and the assistant's reply).

Rules:
- 2-5 words, plain text.
- No surrounding quotes, no trailing punctuation, no emoji.
- Describe what the conversation is about.
- Respond with the title text only, nothing else.`

// TopicNamer calls a cheap model to turn a dialog's first exchange into a short title,
// the same way chat UIs auto-title a new conversation. Shares the client/model-plumbing
// pattern already used by Extractor and Summarizer (both reuse the configured "cheap"
// extractor model too).
type TopicNamer struct {
	client *openai.Client
	model  string
}

func NewTopicNamer(client *openai.Client, model string) *TopicNamer {
	return &TopicNamer{client: client, model: model}
}

// NameTopic returns a short topic title derived from exchange (the dialog's first
// exchange, rendered as plain text by the caller). Returns "" (not an error) when the
// model responds with nothing usable.
func (n *TopicNamer) NameTopic(ctx context.Context, exchange string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, topicNamerTimeout)
	defer cancel()

	resp, err := n.client.Responses.New(ctx, responses.ResponseNewParams{
		Model:        shared.ResponsesModel(n.model),
		Instructions: openai.String(topicNamerSystemPrompt),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(exchange),
		},
	})
	if err != nil {
		slog.ErrorContext(ctx, "topic namer: completion failed", "model", n.model, "error", err)
		return "", err
	}
	name := strings.Trim(strings.TrimSpace(resp.OutputText()), "\"'")
	if name == "" {
		return "", nil
	}
	// Truncate by rune, not byte, so a multi-byte UTF-8 character at the cut point
	// doesn't get split into an invalid partial sequence.
	if runes := []rune(name); len(runes) > telegramTopicNameLimit {
		name = strings.TrimSpace(string(runes[:telegramTopicNameLimit]))
	}
	return name, nil
}
