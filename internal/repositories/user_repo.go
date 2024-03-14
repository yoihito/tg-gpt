package repositories

import (
	"sync"
	"time"

	"vadimgribanov.com/tg-gpt/internal/models"
)

type UserRepo struct {
	users []models.User
	lock  sync.RWMutex
}

func NewUserRepo() *UserRepo {
	return &UserRepo{users: []models.User{}}
}

func (repo *UserRepo) Register(userId int64, firstName string, lastName string, username string, chatId int64, active bool) (models.User, error) {
	repo.lock.Lock()
	defer repo.lock.Unlock()
	newUser := models.User{
		Id:              userId,
		FirstName:       firstName,
		LastName:        lastName,
		Username:        username,
		ChatId:          chatId,
		LastInteraction: time.Now().Unix(),
		Active:          active,
		CurrentModel:    "openai",
	}
	repo.users = append(repo.users, newUser)
	return newUser, nil
}

func (repo *UserRepo) CheckIfUserExists(userId int64) bool {
	repo.lock.RLock()
	defer repo.lock.RUnlock()
	for _, user := range repo.users {
		if user.Id == userId {
			return true
		}
	}
	return false
}

func (repo *UserRepo) GetUser(userId int64) (models.User, error) {
	repo.lock.RLock()
	defer repo.lock.RUnlock()
	for _, user := range repo.users {
		if user.Id == userId {
			return user, nil
		}
	}
	return models.User{}, nil
}

func (repo *UserRepo) UpdateUser(user models.User) error {
	repo.lock.Lock()
	defer repo.lock.Unlock()
	for i, u := range repo.users {
		if u.Id == user.Id {
			repo.users[i] = user
			return nil
		}
	}
	return nil
}
