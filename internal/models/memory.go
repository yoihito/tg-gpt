package models

type Memory struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	MemoryKey   string `json:"memory_key"`
	MemoryValue string `json:"memory_value"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}
