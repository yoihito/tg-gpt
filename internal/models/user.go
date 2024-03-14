package models

import "time"

type User struct {
	Id                   int64
	FirstName            string
	LastName             string
	Username             string
	ChatId               int64
	TranscribedSeconds   int64
	NumberOfInputTokens  int64
	NumberOfOutputTokens int64
	CurrentDialogId      int64
	LastInteraction      int64
	Active               bool
	CurrentModel         string
}

func (u *User) StartNewDialog() {
	u.CurrentDialogId++
}

func (u *User) Touch() {
	u.LastInteraction = time.Now().Unix()
}
