package main

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
	"gopkg.in/telebot.v3/middleware"
	"vadimgribanov.com/tg-gpt/internal/handlers"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/user_middleware"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file")
	}
	client := openai.NewClient(os.Getenv("OPENAI_API_KEY"))
	messagesRepo := repositories.NewMessagesRepo()
	allowedUserId, err := strconv.ParseInt(os.Getenv("ALLOWED_USER_ID"), 10, 0)
	userRepo := repositories.NewUserRepo(allowedUserId)
	dialogTimeout, err := strconv.ParseInt(os.Getenv("DIALOG_TIMEOUT"), 10, 0)
	if err != nil {
		log.Fatal(err)
		return
	}
	textHandler := handlers.TextHandler{
		Client:        client,
		MessagesRepo:  messagesRepo,
		UsersRepo:     userRepo,
		DialogTimeout: dialogTimeout,
	}
	voiceHandler := handlers.VoiceHandler{TextHandler: textHandler, Client: client}

	pref := tele.Settings{
		Token:  os.Getenv("TOKEN"),
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatal(err)
		return
	}
	b.Use(middleware.Logger())
	b.Use(user_middleware.AuthenticateUser(userRepo))

	b.Handle("/start", func(c tele.Context) error {
		return c.Send("Hello! I'm a bot that can talk to you. Just send me a voice message or text and I will respond to you.")
	})

	b.Handle("/ping", func(c tele.Context) error {
		return c.Send("pong")
	})

	b.Handle("/retry", func(c tele.Context) error {
		placeholderMessage, err := c.Bot().Send(c.Recipient(), "...")
		if err != nil {
			return err
		}
		err = c.Notify(tele.Typing)
		if err != nil {
			return err
		}

		user := c.Get("user").(models.User)
		interaction := messagesRepo.PopLatestInteraction(user)
		gptResponse, err := textHandler.OnTextHandler(user, interaction.UserMessage)
		if err != nil {
			return err
		}

		_, err = c.Bot().Edit(placeholderMessage, gptResponse, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
		if err != nil {
			return err
		}
		return nil
	})

	b.Handle(tele.OnVoice, func(c tele.Context) error {
		voiceFile := c.Message().Voice

		reader, err := c.Bot().File(&voiceFile.File)
		if err != nil {
			return err
		}
		defer reader.Close()

		user := c.Get("user").(models.User)
		responseMessages, cancel := voiceHandler.OnVoiceHandler(user, reader)
		defer cancel()

		for message := range responseMessages {
			if message.Err != nil {
				return message.Err
			}
			err = c.Send(message.Text, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
			if err != nil {
				return err
			}
		}
		return nil
	})

	b.Handle(tele.OnText, func(c tele.Context) error {
		placeholderMessage, err := c.Bot().Send(c.Recipient(), "...")
		if err != nil {
			return err
		}
		err = c.Notify(tele.Typing)
		if err != nil {
			return err
		}
		userInput := c.Message().Text
		user := c.Get("user").(models.User)
		gptResponse, err := textHandler.OnTextHandler(user, userInput)
		if err != nil {
			return err
		}

		_, err = c.Bot().Edit(placeholderMessage, gptResponse, &tele.SendOptions{ParseMode: tele.ModeMarkdown})
		if err != nil {
			return err
		}
		return nil
	})

	b.Start()
}
