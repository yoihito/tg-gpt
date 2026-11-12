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

const topicNamerSystemPrompt = `Generate a short title for a chat topic, based on the user's first message in it.

Rules:
- 2-5 words, plain text.
- No surrounding quotes, no trailing punctuation, no emoji.
- Describe what the conversation is about, not the assistant's reply.
- Respond with the title text only, nothing else.`

// TopicNamer calls a cheap model to turn a dialog's first message into a short title,
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

// NameTopic returns a short topic title derived from firstUserMessage. Returns ""
// (not an error) when the model responds with nothing usable.
func (n *TopicNamer) NameTopic(ctx context.Context, firstUserMessage string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, topicNamerTimeout)
	defer cancel()

	resp, err := n.client.Responses.New(ctx, responses.ResponseNewParams{
		Model:        shared.ResponsesModel(n.model),
		Instructions: openai.String(topicNamerSystemPrompt),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(firstUserMessage),
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
	if len(name) > telegramTopicNameLimit {
		name = name[:telegramTopicNameLimit]
	}
	return name, nil
}
