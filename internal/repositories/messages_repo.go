package repositories

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type MessagesRepo struct {
	db *database.DB
}

func NewMessagesRepo(db *database.DB) *MessagesRepo {
	return &MessagesRepo{db: db}
}

func (repo *MessagesRepo) AddMessage(message models.Interaction) error {
	tx, err := repo.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert interaction
	interactionQuery := `
		INSERT INTO interactions (author_id, dialog_id, tg_user_message_id, tg_assistant_message_id)
		VALUES (?, ?, ?, ?)
	`
	
	result, err := tx.Exec(interactionQuery, message.AuthorId, message.DialogId, message.TgUserMessageId, message.TgAssistantMessageId)
	if err != nil {
		return fmt.Errorf("failed to insert interaction: %w", err)
	}
	
	interactionID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get interaction ID: %w", err)
	}

	// Insert user message
	if err := repo.insertMessage(tx, interactionID, "user", message.UserMessage); err != nil {
		return fmt.Errorf("failed to insert user message: %w", err)
	}

	// Insert assistant message
	if err := repo.insertMessage(tx, interactionID, "assistant", message.AssistantMessage); err != nil {
		return fmt.Errorf("failed to insert assistant message: %w", err)
	}

	return tx.Commit()
}

func (repo *MessagesRepo) GetCurrentDialogForUser(user models.User) ([]models.Interaction, error) {
	query := `
		SELECT i.id, i.author_id, i.dialog_id, i.tg_user_message_id, i.tg_assistant_message_id
		FROM interactions i
		WHERE i.author_id = ? AND i.dialog_id = ?
		ORDER BY i.created_at ASC
	`
	
	rows, err := repo.db.Query(query, user.Id, user.CurrentDialogId)
	if err != nil {
		return nil, fmt.Errorf("failed to query interactions: %w", err)
	}
	defer rows.Close()
	
	var interactions []models.Interaction
	for rows.Next() {
		var interactionID int64
		var interaction models.Interaction
		
		err := rows.Scan(&interactionID, &interaction.AuthorId, &interaction.DialogId,
			&interaction.TgUserMessageId, &interaction.TgAssistantMessageId)
		if err != nil {
			return nil, fmt.Errorf("failed to scan interaction: %w", err)
		}
		
		// Load messages for this interaction
		if err := repo.loadMessages(interactionID, &interaction); err != nil {
			return nil, fmt.Errorf("failed to load messages: %w", err)
		}
		
		interactions = append(interactions, interaction)
	}
	
	return interactions, nil
}

func (repo *MessagesRepo) PopLatestInteraction(user models.User) (models.Interaction, error) {
	tx, err := repo.db.Begin()
	if err != nil {
		return models.Interaction{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Find the latest interaction
	query := `
		SELECT i.id, i.author_id, i.dialog_id, i.tg_user_message_id, i.tg_assistant_message_id
		FROM interactions i
		WHERE i.author_id = ?
		ORDER BY i.created_at DESC
		LIMIT 1
	`
	
	var interactionID int64
	var interaction models.Interaction
	
	err = tx.QueryRow(query, user.Id).Scan(&interactionID, &interaction.AuthorId,
		&interaction.DialogId, &interaction.TgUserMessageId, &interaction.TgAssistantMessageId)
	if err != nil {
		if err == sql.ErrNoRows {
			return models.Interaction{}, errors.New("no messages found")
		}
		return models.Interaction{}, fmt.Errorf("failed to find latest interaction: %w", err)
	}
	
	// Load messages for this interaction
	if err := repo.loadMessagesWithTx(tx, interactionID, &interaction); err != nil {
		return models.Interaction{}, fmt.Errorf("failed to load messages: %w", err)
	}
	
	// Delete the interaction and its messages
	if _, err := tx.Exec("DELETE FROM messages WHERE interaction_id = ?", interactionID); err != nil {
		return models.Interaction{}, fmt.Errorf("failed to delete messages: %w", err)
	}
	
	if _, err := tx.Exec("DELETE FROM interactions WHERE id = ?", interactionID); err != nil {
		return models.Interaction{}, fmt.Errorf("failed to delete interaction: %w", err)
	}
	
	if err := tx.Commit(); err != nil {
		return models.Interaction{}, fmt.Errorf("failed to commit transaction: %w", err)
	}
	
	return interaction, nil
}

func (repo *MessagesRepo) insertMessage(tx *sql.Tx, interactionID int64, role string, message openai.ChatCompletionMessage) error {
	var content string
	var multiContent sql.NullString
	
	// Handle multi-content messages first
	if len(message.MultiContent) > 0 {
		jsonData, err := json.Marshal(message.MultiContent)
		if err != nil {
			return fmt.Errorf("failed to serialize multi-content: %w", err)
		}
		multiContent = sql.NullString{String: string(jsonData), Valid: true}
		// When MultiContent is used, content should be empty in the DB
		content = ""
	} else {
		// Handle regular content - ensure it's never empty
		if message.Content == "" {
			content = " " // Use space as fallback for empty content
		} else {
			content = message.Content
		}
	}
	
	query := `INSERT INTO messages (interaction_id, role, content, multi_content) VALUES (?, ?, ?, ?)`
	_, err := tx.Exec(query, interactionID, role, content, multiContent)
	return err
}

func (repo *MessagesRepo) loadMessages(interactionID int64, interaction *models.Interaction) error {
	query := `SELECT role, content, multi_content FROM messages WHERE interaction_id = ? ORDER BY created_at ASC`
	rows, err := repo.db.Query(query, interactionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	
	for rows.Next() {
		var role, content string
		var multiContent sql.NullString
		
		if err := rows.Scan(&role, &content, &multiContent); err != nil {
			return err
		}
		
		if role == "user" {
			interaction.UserMessage.Role = "user"
			if multiContent.Valid {
				var multiContentData []openai.ChatMessagePart
				if err := json.Unmarshal([]byte(multiContent.String), &multiContentData); err == nil {
					interaction.UserMessage.MultiContent = multiContentData
					// When MultiContent is used, Content should be empty
					interaction.UserMessage.Content = ""
				} else {
					// Fallback to regular content if MultiContent parsing fails
					interaction.UserMessage.Content = content
				}
			} else {
				// Ensure content is never empty/null
				if content == "" {
					interaction.UserMessage.Content = " " // Use space as fallback
				} else {
					interaction.UserMessage.Content = content
				}
			}
		} else if role == "assistant" {
			interaction.AssistantMessage.Role = "assistant"
			if multiContent.Valid {
				var multiContentData []openai.ChatMessagePart
				if err := json.Unmarshal([]byte(multiContent.String), &multiContentData); err == nil {
					interaction.AssistantMessage.MultiContent = multiContentData
					interaction.AssistantMessage.Content = ""
				} else {
					interaction.AssistantMessage.Content = content
				}
			} else {
				if content == "" {
					interaction.AssistantMessage.Content = " " // Use space as fallback
				} else {
					interaction.AssistantMessage.Content = content
				}
			}
		}
	}
	
	return nil
}

func (repo *MessagesRepo) loadMessagesWithTx(tx *sql.Tx, interactionID int64, interaction *models.Interaction) error {
	query := `SELECT role, content, multi_content FROM messages WHERE interaction_id = ? ORDER BY created_at ASC`
	rows, err := tx.Query(query, interactionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	
	for rows.Next() {
		var role, content string
		var multiContent sql.NullString
		
		if err := rows.Scan(&role, &content, &multiContent); err != nil {
			return err
		}
		
		if role == "user" {
			interaction.UserMessage.Role = "user"
			if multiContent.Valid {
				var multiContentData []openai.ChatMessagePart
				if err := json.Unmarshal([]byte(multiContent.String), &multiContentData); err == nil {
					interaction.UserMessage.MultiContent = multiContentData
					// When MultiContent is used, Content should be empty
					interaction.UserMessage.Content = ""
				} else {
					// Fallback to regular content if MultiContent parsing fails
					interaction.UserMessage.Content = content
				}
			} else {
				// Ensure content is never empty/null
				if content == "" {
					interaction.UserMessage.Content = " " // Use space as fallback
				} else {
					interaction.UserMessage.Content = content
				}
			}
		} else if role == "assistant" {
			interaction.AssistantMessage.Role = "assistant"
			if multiContent.Valid {
				var multiContentData []openai.ChatMessagePart
				if err := json.Unmarshal([]byte(multiContent.String), &multiContentData); err == nil {
					interaction.AssistantMessage.MultiContent = multiContentData
					interaction.AssistantMessage.Content = ""
				} else {
					interaction.AssistantMessage.Content = content
				}
			} else {
				if content == "" {
					interaction.AssistantMessage.Content = " " // Use space as fallback
				} else {
					interaction.AssistantMessage.Content = content
				}
			}
		}
	}
	
	return nil
}
