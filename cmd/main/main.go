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
	tele_middleware "gopkg.in/telebot.v3/middleware"
	"vadimgribanov.com/tg-gpt/internal/adapters"
	"vadimgribanov.com/tg-gpt/internal/handlers"
	"vadimgribanov.com/tg-gpt/internal/middleware"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/telegram_utils"
	"vadimgribanov.com/tg-gpt/internal/vendors/anthropic"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file")
	}
	client := openai.NewClient(os.Getenv("OPENAI_API_KEY"))
	anthropicClient := anthropic.NewClient(os.Getenv("ANTHROPIC_API_KEY"))
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
	rateLimiter := middleware.RateLimiter{MaxConcurrentRequests: maxConcurrentRequests}
	authenticator := middleware.UserAuthenticator{UserRepo: userRepo, AllowedUserIds: allowedUserIDs}
	llmClientFactory := handlers.NewLLMClientFactory()
	llmClientFactory.RegisterClient("openai", adapters.NewOpenaiAdapter(client))
	llmClientFactory.RegisterClient("anthropic", adapters.NewAnthropicAdapter(anthropicClient))
	textHandlerFactory := handlers.TextHandlerFactory{
		ClientFactory: llmClientFactory,
		MessagesRepo:  messagesRepo,
		UsersRepo:     userRepo,
		DialogTimeout: dialogTimeout,
	}
	voiceHandler := handlers.VoiceHandler{
		Client: client,
	}

	pref := tele.Settings{
		Token:  os.Getenv("TOKEN"),
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatal(err)
		return
	}
	b.Use(tele_middleware.Logger())
	b.Use(authenticator.Middleware())
	b.Handle("/cancel", func(c tele.Context) error {
		rateLimiter.CancelRequest(c.Get("user").(models.User))
		return nil
	})

	limitedGroup := b.Group()

	limitedGroup.Use(rateLimiter.Middleware())

	limitedGroup.Handle("/start", func(c tele.Context) error {
		return c.Send("Hello! I'm a bot that can talk to you. Just send me a voice message or text and I will respond to you.")
	})

	limitedGroup.Handle("/ping", func(c tele.Context) error {
		return c.Send("pong")
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

	limitedGroup.Handle("/retry", func(c tele.Context) error {
		ctx := c.Get("requestContext").(context.Context)

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
		textHandler, err := textHandlerFactory.NewTextHandler(user, interaction.TgUserMessageId)
		if err != nil {
			return err
		}
		chunksCh := textHandler.OnStreamableTextHandler(ctx, interaction.UserMessage)

		return telegram_utils.SendStream(c, &tele.Message{
			ID:   int(interaction.TgUserMessageId),
			Chat: c.Chat(),
		}, chunksCh)
	})

	limitedGroup.Handle("/change_model", func(c tele.Context) error {
		user := c.Get("user").(models.User)
		user.CurrentModel = c.Args()[0]
		err := userRepo.UpdateUser(user)
		if err != nil {
			return err
		}
		return c.Send(fmt.Sprintf("Model changed to %s", c.Args()[0]))
	})

	limitedGroup.Handle(tele.OnVoice, func(c tele.Context) error {
		ctx := c.Get("requestContext").(context.Context)
		voiceFile := c.Message().Voice

		reader, err := c.Bot().File(&voiceFile.File)
		if err != nil {
			return err
		}
		defer reader.Close()

		user := c.Get("user").(models.User)
		transcriptionText, err := voiceHandler.OnVoiceHandler(ctx, reader)
		if err != nil {
			c.Reply("Failed to transcribe voice message")
			return err
		}

		err = c.Reply(fmt.Sprintf("Transcription: _%s_", transcriptionText), &tele.SendOptions{
			ParseMode: tele.ModeMarkdown,
		})
		if err != nil {
			return err
		}

		textHandler, err := textHandlerFactory.NewTextHandler(user, int64(c.Message().ID))
		if err != nil {
			return err
		}
		chunksCh := textHandler.OnStreamableTextHandler(ctx, transcriptionText)

		return telegram_utils.SendStream(c, c.Message(), chunksCh)
	})

	limitedGroup.Handle(tele.OnText, func(c tele.Context) error {
		log.Println("Got text message")
		ctx := c.Get("requestContext").(context.Context)
		user := c.Get("user").(models.User)

		err = c.Notify(tele.Typing)
		if err != nil {
			return err
		}
		userInput := c.Message().Text
		textHandler, err := textHandlerFactory.NewTextHandler(user, int64(c.Message().ID))
		if err != nil {
			return err
		}
		chunksCh := textHandler.OnStreamableTextHandler(ctx, userInput)
		return telegram_utils.SendStream(c, c.Message(), chunksCh)
	})

	log.Println("Listening...")
	b.Start()
}
