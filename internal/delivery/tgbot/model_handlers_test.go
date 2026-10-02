package tgbot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/config"
	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/services"
)

type modelContext struct {
	tele.Context
	sent      interface{}
	options   *tele.SendOptions
	responded bool
	response  *tele.CallbackResponse
}

func (c *modelContext) Send(what interface{}, opts ...interface{}) error {
	c.sent = what
	for _, opt := range opts {
		if options, ok := opt.(*tele.SendOptions); ok {
			c.options = options
		}
	}
	return nil
}

func (c *modelContext) Respond(responses ...*tele.CallbackResponse) error {
	c.responded = true
	if len(responses) > 0 {
		c.response = responses[0]
	}
	return nil
}

func TestModelSelection(t *testing.T) {
	db, err := database.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	repo := repositories.NewUserRepo(db)
	user, err := repo.Register(1, "Test", "", "", 10, true, "first")
	if err != nil {
		t.Fatal(err)
	}
	handler := &BotHandler{userRepo: repo, llmClientProxy: services.NewClientProxyFromConfig(&config.Config{Models: []config.LLMModel{
		{ModelId: "first", Provider: "openai"}, {ModelId: "second", Provider: "openai"},
	}})}
	newContext := func(data string) *modelContext {
		c := &modelContext{Context: (&tele.Bot{}).NewContext(tele.Update{Callback: &tele.Callback{
			ID: "callback", Data: data, Sender: &tele.User{ID: 1},
			Message: &tele.Message{ID: 5, ThreadID: 42, Chat: &tele.Chat{ID: 10}},
		}})}
		c.Set("requestContext", context.Background())
		c.Set("user", user)
		return c
	}
	t.Run("menu stays in topic", func(t *testing.T) {
		c := newContext("")
		if err := handler.ListModels(c); err != nil {
			t.Fatal(err)
		}
		if c.options == nil || c.options.ThreadID != 42 || c.options.ReplyMarkup == nil {
			t.Fatalf("missing topic or menu: %#v", c.options)
		}
		buttons := c.options.ReplyMarkup.InlineKeyboard
		if len(buttons) != 2 {
			t.Fatalf("got %d model buttons", len(buttons))
		}
		for _, row := range buttons {
			if row[0].Unique != "model" {
				t.Fatal("incorrect callback routing")
			}
		}
	})
	t.Run("selection persists and acknowledges", func(t *testing.T) {
		c := newContext("second")
		if err := handler.ChangeModel(c); err != nil {
			t.Fatal(err)
		}
		stored, err := repo.GetUser(1)
		if err != nil {
			t.Fatal(err)
		}
		if stored.CurrentModel != "second" || c.Get("user").(models.User).CurrentModel != "second" {
			t.Fatal("selection not persisted")
		}
		if !c.responded || c.options == nil || c.options.ThreadID != 42 {
			t.Fatal("callback not acknowledged in topic")
		}
	})
	t.Run("stale and malformed selections", func(t *testing.T) {
		for _, data := range []string{"", "retired", "second|unexpected"} {
			c := newContext(data)
			if err := handler.ChangeModel(c); err != nil {
				t.Fatal(err)
			}
			if c.response == nil || !strings.Contains(c.response.Text, "/change_model") {
				t.Fatal("missing stale selection notice")
			}
		}
		stored, err := repo.GetUser(1)
		if err != nil {
			t.Fatal(err)
		}
		if stored.CurrentModel != "second" {
			t.Fatal("invalid callback changed model")
		}
	})
}
