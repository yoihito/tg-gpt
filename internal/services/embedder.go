package services

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/openai/openai-go/v3"
	"vadimgribanov.com/tg-gpt/internal/vec"
)

const openaiEmbeddingTimeout = 30 * time.Second

type Embedder struct {
	client *openai.Client
	model  string
}

func NewEmbedder(client *openai.Client, model string) *Embedder {
	return &Embedder{client: client, model: model}
}

func (e *Embedder) Model() string { return e.model }

// Embed returns an L2-normalized embedding for the given text. Cosine similarity
// against another normalized vector is then just a dot product.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("embed: empty text")
	}
	ctx, cancel := context.WithTimeout(ctx, openaiEmbeddingTimeout)
	defer cancel()

	slog.InfoContext(ctx, "OpenAI embedding: starting",
		"model", e.model,
		"text_len", len(text),
		"timeout", openaiEmbeddingTimeout.String(),
	)
	resp, err := e.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{text}},
		Model: openai.EmbeddingModel(e.model),
	})
	if err != nil {
		slog.ErrorContext(ctx, "OpenAI embedding: failed", "model", e.model, "error", err)
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("openai embed: empty data")
	}
	slog.InfoContext(ctx, "OpenAI embedding: completed",
		"model", e.model,
		"dimensions", len(resp.Data[0].Embedding),
	)
	return vec.Normalize(float32s(resp.Data[0].Embedding)), nil
}

func float32s(in []float64) []float32 {
	out := make([]float32, len(in))
	for i, v := range in {
		out[i] = float32(v)
	}
	return out
}
