package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
	"gopkg.in/telebot.v3/middleware"
	"vadimgribanov.com/tg-gpt/internal/handlers"
	internal_middleware "vadimgribanov.com/tg-gpt/internal/middleware"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/telegram_utils"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file")
	}
	client := openai.NewClient(os.Getenv("OPENAI_API_KEY"))
	messagesRepo := repositories.NewMessagesRepo()

	allowedUserIDsStr := os.Getenv("ALLOWED_USER_ID")
	allowedUserIDs := make([]int64, 0)
	for _, idStr := range strings.Split(allowedUserIDsStr, ",") {
		id, err := strconv.ParseInt(idStr, 10, 0)
		if err != nil {
			log.Fatal(err)
			return
		}
		allowedUserIDs = append(allowedUserIDs, id)
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
	authenticator := internal_middleware.UserAuthenticator{UserRepo: userRepo, AllowedUserIds: allowedUserIDs}
	textHandlerFactory := handlers.TextHandlerFactory{
		Client:        client,
		MessagesRepo:  messagesRepo,
		UsersRepo:     userRepo,
		DialogTimeout: dialogTimeout,
	}
	voiceHandler := handlers.VoiceHandler{
		Client: client,
	}

	pref := tele.Settings{
		Token:     os.Getenv("TOKEN"),
		Poller:    &tele.LongPoller{Timeout: 10 * time.Second},
		ParseMode: tele.ModeMarkdown,
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatal(err)
		return
	}
	b.Use(middleware.Logger())
	b.Use(authenticator.Middleware())
	b.Use(rateLimiter.Middleware())
	limitedGroup := b.Group()

	limitedGroup.Handle("/start", func(c tele.Context) error {
		return c.Send("Hello! I'm a bot that can talk to you. Just send me a voice message or text and I will respond to you.")
	})

	limitedGroup.Handle("/ping", func(c tele.Context) error {
		return c.Send("pong")
	})

	limitedGroup.Handle("/retry", func(c tele.Context) error {
		err = c.Notify(tele.Typing)
		if err != nil {
			return err
		}

		user := c.Get("user").(models.User)
		interaction, err := messagesRepo.PopLatestInteraction(user)
		if err != nil {
			err = c.Send("No messages found")
			if err != nil {
				return err
			}
			return nil
		}

		log.Printf("Interaction: %+v\n", interaction)
		textHandler := textHandlerFactory.NewTextHandler(user, interaction.TgUserMessageId)
		gptResponse, err := textHandler.OnTextHandler(interaction.UserMessage)
		if err != nil {
			return err
		}

		_, err = c.Bot().Reply(&tele.Message{
			ID:   int(interaction.TgUserMessageId),
			Chat: c.Chat(),
		}, gptResponse)
		if err != nil {
			return err
		}
		return nil
	})

	limitedGroup.Handle("/reset", func(c tele.Context) error {
		user := c.Get("user").(models.User)
		user.StartNewDialog()
		err := userRepo.UpdateUser(user)
		if err != nil {
			return err
		}
		return c.Send("New dialog started")
	})

	limitedGroup.Handle(tele.OnVoice, func(c tele.Context) error {
		voiceFile := c.Message().Voice

		reader, err := c.Bot().File(&voiceFile.File)
		if err != nil {
			return err
		}
		defer reader.Close()

		user := c.Get("user").(models.User)
		transcriptionText, err := voiceHandler.OnVoiceHandler(context.Background(), reader)
		if err != nil {
			c.Reply("Failed to transcribe voice message")
			return err
		}

		err = c.Reply(fmt.Sprintf("Transcription: _%s_", transcriptionText))
		if err != nil {
			return err
		}

		textHandler := textHandlerFactory.NewTextHandler(user, int64(c.Message().ID))
		botResponse, err := textHandler.OnTextHandler(transcriptionText)

		if err != nil {
			return err
		}

		return c.Reply(botResponse)
	})

	limitedGroup.Handle(tele.OnText, func(c tele.Context) error {
		ctx, _ := context.WithCancel(context.Background())
		user := c.Get("user").(models.User)

		err = c.Notify(tele.Typing)
		if err != nil {
			return err
		}
		userInput := c.Message().Text
		textHandler := textHandlerFactory.NewTextHandler(user, int64(c.Message().ID))

		chunksCh := textHandler.OnStreamableTextHandler(ctx, userInput)
		commandsCh := telegram_utils.ShapeStream(chunksCh)
		var currentMessage *tele.Message
		for command := range commandsCh {
			log.Printf("Command: %+v\n", command)
			if command.Err != nil {
				return command.Err
			}
			if command.Command == "start" {
				currentMessage, err = c.Bot().Reply(c.Message(), command.Content)
			} else if command.Command == "edit" {
				_, err = c.Bot().Edit(currentMessage, command.Content)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})

	b.Start()
}
