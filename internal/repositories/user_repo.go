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

func (repo *UserRepo) Touch(userID int64, ts int64) error {
	_, err := repo.db.Exec(
		`UPDATE users
		 SET last_interaction = ?, updated_at = strftime('%s', 'now')
		 WHERE id = ?`,
		ts, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to touch user: %w", err)
	}
	return nil
}

func (repo *UserRepo) AddTokenUsage(userID int64, inputTokens, outputTokens int64) error {
	_, err := repo.db.Exec(
		`UPDATE users
		 SET number_of_input_tokens = number_of_input_tokens + ?,
		     number_of_output_tokens = number_of_output_tokens + ?,
		     updated_at = strftime('%s', 'now')
		 WHERE id = ?`,
		inputTokens, outputTokens, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to add token usage: %w", err)
	}
	return nil
}

func (repo *UserRepo) SetCurrentModel(userID int64, model string) error {
	_, err := repo.db.Exec(
		`UPDATE users
		 SET current_model = ?, updated_at = strftime('%s', 'now')
		 WHERE id = ?`,
		model, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to set current model: %w", err)
	}
	return nil
}

func (repo *UserRepo) StartNewDialogCAS(userID, expectedDialogID, ts int64) (int64, bool, error) {
	res, err := repo.db.Exec(
		`UPDATE users
		 SET current_dialog_id = current_dialog_id + 1,
		     last_interaction = ?,
		     updated_at = strftime('%s', 'now')
		 WHERE id = ? AND current_dialog_id = ?`,
		ts, userID, expectedDialogID,
	)
	if err != nil {
		return 0, false, fmt.Errorf("failed to start new dialog: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if affected == 0 {
		return 0, false, nil
	}
	return expectedDialogID + 1, true, nil
}
