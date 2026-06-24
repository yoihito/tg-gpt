package services

import (
	"context"
	"fmt"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/vec"
)

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
	resp, err := e.client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
		Input: []string{text},
		Model: openai.EmbeddingModel(e.model),
	})
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("openai embed: empty data")
	}
	return vec.Normalize(resp.Data[0].Embedding), nil
}
