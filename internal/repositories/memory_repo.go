package repositories

import (
	"database/sql"
	"fmt"
	"time"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type MemoryRepo struct {
	db *database.DB
}

func NewMemoryRepo(db *database.DB) *MemoryRepo {
	return &MemoryRepo{db: db}
}

func (repo *MemoryRepo) SaveMemory(userID int64, key, value string) error {
	now := time.Now().Unix()

	query := `
		INSERT INTO memories (user_id, memory_key, memory_value, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, memory_key) DO UPDATE SET
			memory_value = excluded.memory_value,
			updated_at = excluded.updated_at
	`

	_, err := repo.db.Exec(query, userID, key, value, now, now)
	if err != nil {
		return fmt.Errorf("failed to save memory: %w", err)
	}

	return nil
}

func (repo *MemoryRepo) GetMemory(userID int64, key string) (*models.Memory, error) {
	query := `
		SELECT id, user_id, memory_key, memory_value, created_at, updated_at
		FROM memories 
		WHERE user_id = ? AND memory_key = ?
	`

	var memory models.Memory
	err := repo.db.QueryRow(query, userID, key).Scan(
		&memory.ID, &memory.UserID, &memory.MemoryKey, &memory.MemoryValue,
		&memory.CreatedAt, &memory.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Memory not found
		}
		return nil, fmt.Errorf("failed to get memory: %w", err)
	}

	return &memory, nil
}

func (repo *MemoryRepo) GetUserMemories(userID int64) ([]models.Memory, error) {
	query := `
		SELECT id, user_id, memory_key, memory_value, created_at, updated_at
		FROM memories 
		WHERE user_id = ?
		ORDER BY updated_at DESC
	`

	rows, err := repo.db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query memories: %w", err)
	}
	defer rows.Close()

	var memories []models.Memory
	for rows.Next() {
		var memory models.Memory
		err := rows.Scan(
			&memory.ID, &memory.UserID, &memory.MemoryKey, &memory.MemoryValue,
			&memory.CreatedAt, &memory.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan memory: %w", err)
		}
		memories = append(memories, memory)
	}

	return memories, nil
}

func (repo *MemoryRepo) DeleteMemory(userID int64, key string) error {
	query := `DELETE FROM memories WHERE user_id = ? AND memory_key = ?`
	_, err := repo.db.Exec(query, userID, key)
	if err != nil {
		return fmt.Errorf("failed to delete memory: %w", err)
	}
	return nil
}
