package user_middleware

import (
	"log"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type UserRepo interface {
	Register(int64, string, string, string, int64, bool) (models.User, error)
	CheckIfUserExists(int64) bool
	GetUser(int64) (models.User, error)
}

func AuthenticateUser(userRepo UserRepo, allowedUserId int64) tele.MiddlewareFunc {
	l := log.Default()
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			user := models.User{}
			if !userRepo.CheckIfUserExists(c.Sender().ID) {
				userId := c.Sender().ID
				firstName := c.Sender().FirstName
				lastName := c.Sender().LastName
				username := c.Sender().Username
				chatId := c.Update().Message.Chat.ID
				user, _ = userRepo.Register(userId, firstName, lastName, username, chatId, userId == allowedUserId)
			} else {
				user, _ = userRepo.GetUser(c.Sender().ID)
			}
			l.Printf("User: %+v\n", user)
			if user.Active {
				c.Set("user", user)
				return next(c)
			}
			c.Send("You are not registered. Please contact the administrator.")
			return nil
		}
	}
}
