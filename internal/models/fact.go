package models

const (
	FactStatusActive     = "active"
	FactStatusSuperseded = "superseded"
	FactStatusRevoked    = "revoked"
)

type Fact struct {
	ID             int64
	UserID         int64
	Subject        string
	Content        string
	ContentHash    string
	Confidence     float64
	Status         string
	SupersedesID   *int64
	SourceTraceID  int64
	Embedding      []float32
	EmbeddingModel string
	CreatedAt      int64
}
