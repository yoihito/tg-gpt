package middleware

import (
	"context"
	"sync"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type RateLimiter struct {
	Locks                 sync.Map
	MaxConcurrentRequests int
}

type userRequestManager struct {
	cancelFunc  context.CancelFunc
	mu          sync.Mutex
	limiterChan chan struct{}
}

func (r *RateLimiter) Middleware() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			ctx := c.Get("requestContext").(context.Context)

			user := c.Get("user").(models.User)
			userLock, _ := r.Locks.LoadOrStore(user.Id, &userRequestManager{
				limiterChan: make(chan struct{}, r.MaxConcurrentRequests),
			})
			manager := userLock.(*userRequestManager)

			select {
			case manager.limiterChan <- struct{}{}:
				defer func() {
					<-manager.limiterChan
					manager.mu.Lock()
					manager.cancelFunc = nil
					manager.mu.Unlock()
				}()
				ctx, cancel := context.WithCancel(ctx)
				manager.mu.Lock()
				manager.cancelFunc = cancel
				manager.mu.Unlock()
				c.Set("requestContext", ctx)

				return next(c)
			default:
				return c.Send("Please wait for the response from the bot.")
			}
		}
	}
}

func (r *RateLimiter) CancelRequest(user models.User) {
	userLock, ok := r.Locks.Load(user.Id)
	if !ok {
		return
	}
	manager := userLock.(*userRequestManager)
	manager.mu.Lock()
	if manager.cancelFunc != nil {
		manager.cancelFunc()
	}
	manager.mu.Unlock()
}
