package repositories

import (
	"database/sql"
	"fmt"
	"time"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type UserRepo struct {
	db *database.DB
}

func NewUserRepo(db *database.DB) *UserRepo {
	return &UserRepo{db: db}
}

func (repo *UserRepo) Register(userId int64, firstName string, lastName string, username string, chatId int64, active bool, modelId string) (models.User, error) {
	now := time.Now().Unix()
	
	query := `
		INSERT INTO users (id, first_name, last_name, username, chat_id, last_interaction, active, current_model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	
	_, err := repo.db.Exec(query, userId, firstName, lastName, username, chatId, now, active, modelId)
	if err != nil {
		return models.User{}, fmt.Errorf("failed to register user: %w", err)
	}
	
	newUser := models.User{
		Id:              userId,
		FirstName:       firstName,
		LastName:        lastName,
		Username:        username,
		ChatId:          chatId,
		LastInteraction: now,
		Active:          active,
		CurrentModel:    modelId,
	}
	
	return newUser, nil
}

func (repo *UserRepo) CheckIfUserExists(userId int64) bool {
	query := `SELECT COUNT(*) FROM users WHERE id = ?`
	var count int
	err := repo.db.QueryRow(query, userId).Scan(&count)
	return err == nil && count > 0
}

func (repo *UserRepo) GetUser(userId int64) (models.User, error) {
	query := `
		SELECT id, first_name, last_name, username, chat_id, transcribed_seconds, 
			   number_of_input_tokens, number_of_output_tokens, current_dialog_id, 
			   last_interaction, active, current_model
		FROM users WHERE id = ?
	`
	
	var user models.User
	var lastName, username sql.NullString
	
	err := repo.db.QueryRow(query, userId).Scan(
		&user.Id, &user.FirstName, &lastName, &username, &user.ChatId,
		&user.TranscribedSeconds, &user.NumberOfInputTokens, &user.NumberOfOutputTokens,
		&user.CurrentDialogId, &user.LastInteraction, &user.Active, &user.CurrentModel,
	)
	
	if err != nil {
		if err == sql.ErrNoRows {
			return models.User{}, fmt.Errorf("user not found")
		}
		return models.User{}, fmt.Errorf("failed to get user: %w", err)
	}
	
	if lastName.Valid {
		user.LastName = lastName.String
	}
	if username.Valid {
		user.Username = username.String
	}
	
	return user, nil
}

func (repo *UserRepo) UpdateUser(user models.User) error {
	query := `
		UPDATE users 
		SET first_name = ?, last_name = ?, username = ?, chat_id = ?, 
			transcribed_seconds = ?, number_of_input_tokens = ?, number_of_output_tokens = ?,
			current_dialog_id = ?, last_interaction = ?, active = ?, current_model = ?,
			updated_at = strftime('%s', 'now')
		WHERE id = ?
	`
	
	_, err := repo.db.Exec(query, 
		user.FirstName, user.LastName, user.Username, user.ChatId,
		user.TranscribedSeconds, user.NumberOfInputTokens, user.NumberOfOutputTokens,
		user.CurrentDialogId, user.LastInteraction, user.Active, user.CurrentModel,
		user.Id,
	)
	
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}
	
	return nil
}
