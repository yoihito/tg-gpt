package repositories

import (
	"time"

	"vadimgribanov.com/tg-gpt/internal/models"
)

type UserRepo struct {
	users         []models.User
	allowedUserId int64
}

func NewUserRepo(allowedUserId int64) *UserRepo {
	return &UserRepo{users: []models.User{}, allowedUserId: allowedUserId}
}

func (repo *UserRepo) Register(userId int64, firstName string, lastName string, username string, chatId int64) (models.User, error) {
	newUser := models.User{
		Id:              userId,
		FirstName:       firstName,
		LastName:        lastName,
		Username:        username,
		ChatId:          chatId,
		LastInteraction: time.Now().Unix(),
	}
	if userId == repo.allowedUserId {
		newUser.Active = true
	}
	repo.users = append(repo.users, newUser)
	return newUser, nil
}

func (repo *UserRepo) CheckIfUserExists(userId int64) bool {
	for _, user := range repo.users {
		if user.Id == userId {
			return true
		}
	}
	return false
}

func (repo *UserRepo) GetUser(userId int64) (models.User, error) {
	for _, user := range repo.users {
		if user.Id == userId {
			return user, nil
		}
	}
	return models.User{}, nil
}

func (repo *UserRepo) UpdateUser(user models.User) error {
	for i, u := range repo.users {
		if u.Id == user.Id {
			repo.users[i] = user
			return nil
		}
	}
	return nil
}
