package middleware

import (
	"context"
	"log/slog"

	"golang.org/x/exp/slices"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/config"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type UserRepo interface {
	Register(int64, string, string, string, int64, bool, string) (models.User, error)
	CheckIfUserExists(int64) bool
	GetUser(int64) (models.User, error)
}

type UserAuthenticator struct {
	UserRepo       UserRepo
	AllowedUserIds []int64
	AppConfig      config.Config
}

func (u *UserAuthenticator) Middleware() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			ctx := c.Get("requestContext").(context.Context)

			var user models.User
			if !u.UserRepo.CheckIfUserExists(c.Sender().ID) {
				userId := c.Sender().ID
				firstName := c.Sender().FirstName
				lastName := c.Sender().LastName
				username := c.Sender().Username
				chatId := c.Update().Message.Chat.ID
				user, _ = u.UserRepo.Register(
					userId,
					firstName,
					lastName,
					username,
					chatId,
					slices.Contains(u.AllowedUserIds, userId),
					u.AppConfig.DefaultModel.ModelId,
				)
			} else {
				user, _ = u.UserRepo.GetUser(c.Sender().ID)
			}
			slog.DebugContext(ctx, "User authenticated", "user", user)
			if user.Active {
				c.Set("user", user)
				ctx = context.WithValue(ctx, "user_id", user.Id)
				c.Set("requestContext", ctx)
				return next(c)
			}
			c.Send("You are not registered. Please contact the administrator.")
			return nil
		}
	}
}
