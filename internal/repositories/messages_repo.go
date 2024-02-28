package repositories

import (
	"errors"
	"sync"

	"vadimgribanov.com/tg-gpt/internal/models"
)

type MessagesRepo struct {
	messages []models.Interaction
	lock     sync.RWMutex
}

func NewMessagesRepo() *MessagesRepo {
	return &MessagesRepo{messages: make([]models.Interaction, 0)}
}

func (repo *MessagesRepo) AddMessage(message models.Interaction) {
	repo.lock.Lock()
	defer repo.lock.Unlock()
	repo.messages = append(repo.messages, message)

}

func (repo *MessagesRepo) GetCurrentDialogForUser(user models.User) []models.Interaction {
	repo.lock.RLock()
	defer repo.lock.RUnlock()

	currentDialog := make([]models.Interaction, 0)
	for _, message := range repo.messages {
		if message.AuthorId == user.Id && message.DialogId == user.CurrentDialogId {
			currentDialog = append(currentDialog, message)
		}
	}
	return currentDialog
}

func (repo *MessagesRepo) PopLatestInteraction(user models.User) (models.Interaction, error) {
	repo.lock.Lock()
	defer repo.lock.Unlock()

	index := -1
	for i := len(repo.messages) - 1; i >= 0; i-- {
		if repo.messages[i].AuthorId == user.Id {
			index = i
		}
	}
	if index == -1 {
		return models.Interaction{}, errors.New("no messages found")
	}

	latestInteraction := repo.messages[index]
	repo.messages = append(repo.messages[:index], repo.messages[index+1:]...)

	return latestInteraction, nil
}
