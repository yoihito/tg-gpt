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
	internal_middleware "vadimgribanov.com/tg-gpt/internal/middleware"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file")
	}
	client := openai.NewClient(os.Getenv("OPENAI_API_KEY"))
	messagesRepo := repositories.NewMessagesRepo()
	allowedUserId, err := strconv.ParseInt(os.Getenv("ALLOWED_USER_ID"), 10, 0)
	if err != nil {
		log.Fatal(err)
		return
	}
	userRepo := repositories.NewUserRepo()
	dialogTimeout, err := strconv.ParseInt(os.Getenv("DIALOG_TIMEOUT"), 10, 0)
	if err != nil {
		log.Fatal(err)
		return
	}
	maxConcurrentRequests, err := strconv.Atoi(os.Getenv("MAX_CONCURRENT_REQUESTS"))
	if err != nil {
		log.Fatal(err)
		return
	}
	rateLimiter := internal_middleware.RateLimiter{MaxConcurrentRequests: maxConcurrentRequests}
	authenticator := internal_middleware.UserAuthenticator{UserRepo: userRepo, AllowedUserId: allowedUserId}
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
	b.Use(authenticator.Middleware())
	b.Use(rateLimiter.Middleware())

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
		interaction, err := messagesRepo.PopLatestInteraction(user)
		if err != nil {
			_, err = c.Bot().Edit(placeholderMessage, "No messages found")
			if err != nil {
				return err
			}
			return nil
		}

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

	b.Handle("/reset", func(c tele.Context) error {
		user := c.Get("user").(models.User)
		user.StartNewDialog()
		err := userRepo.UpdateUser(user)
		if err != nil {
			return err
		}
		return c.Send("New dialog started")
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
		user := c.Get("user").(models.User)

		placeholderMessage, err := c.Bot().Send(c.Recipient(), "...")
		if err != nil {
			return err
		}
		err = c.Notify(tele.Typing)
		if err != nil {
			return err
		}
		userInput := c.Message().Text
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
