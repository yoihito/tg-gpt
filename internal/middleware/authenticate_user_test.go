package middleware

import (
	"context"
	"errors"
	"testing"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/config"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type callbackUserRepo struct {
	chatID int64
	err    error
}

func (r *callbackUserRepo) CheckIfUserExists(int64) bool       { return false }
func (r *callbackUserRepo) GetUser(int64) (models.User, error) { return models.User{}, r.err }
func (r *callbackUserRepo) Register(id int64, first, last, username string, chatID int64, active bool, model string) (models.User, error) {
	r.chatID = chatID
	return models.User{Id: id, ChatId: chatID, Active: active, CurrentModel: model}, r.err
}

func TestAuthenticateNewUserFromCallback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "register", true: "registration error"}[fail], func(t *testing.T) {
			repo := &callbackUserRepo{}
			if fail {
				repo.err = errors.New("database unavailable")
			}
			auth := &UserAuthenticator{UserRepo: repo, AllowedUserIds: []int64{1}, AppConfig: config.Config{DefaultModel: config.LLMModel{ModelId: "test"}}}
			c := (&tele.Bot{}).NewContext(tele.Update{Callback: &tele.Callback{
				Sender: &tele.User{ID: 1}, Message: &tele.Message{Chat: &tele.Chat{ID: 10}},
			}})
			c.Set("requestContext", context.Background())
			called := false
			err := auth.Middleware()(func(c tele.Context) error {
				called = true
				if c.Get("user").(models.User).CurrentModel != "test" {
					t.Fatal("missing authenticated user")
				}
				return nil
			})(c)
			if !errors.Is(err, repo.err) || called == fail || repo.chatID != 10 {
				t.Fatalf("err=%v called=%v chatID=%d", err, called, repo.chatID)
			}
		})
	}
}
