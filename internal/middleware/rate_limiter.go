package middleware

import (
	"sync"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type RateLimiter struct {
	Locks                 sync.Map
	MaxConcurrentRequests int
}

func (r *RateLimiter) Middleware() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			user := c.Get("user").(models.User)
			userLock, _ := r.Locks.LoadOrStore(user.Id, make(chan struct{}, 1))
			userChan := userLock.(chan struct{})

			select {
			case userChan <- struct{}{}:
				defer func() {
					<-userChan
				}()
				return next(c)
			default:
				return c.Send("Please wait for the response from the bot.")
			}
		}
	}

}
