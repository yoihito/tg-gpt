package models

type Episode struct {
	ID             int64
	UserID         int64
	DialogID       int64
	Summary        string
	StartedAt      int64
	EndedAt        int64
	TurnCount      int64
	Embedding      []float32
	EmbeddingModel string
	CreatedAt      int64
}
