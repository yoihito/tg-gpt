package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
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
	"vadimgribanov.com/tg-gpt/internal/config"
	"vadimgribanov.com/tg-gpt/internal/middleware"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/services"
	"vadimgribanov.com/tg-gpt/internal/telegram_utils"
	"vadimgribanov.com/tg-gpt/internal/vendors/anthropic"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Error loading .env file")
	}
	appConfig, err := config.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	log.Println(appConfig)
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
	authenticator := middleware.UserAuthenticator{UserRepo: userRepo, AllowedUserIds: allowedUserIDs, AppConfig: *appConfig}
	llmClientFactory := services.NewLLMClientFactory()
	llmClientFactory.RegisterProvider("openai", adapters.NewOpenaiAdapter(client))
	llmClientFactory.RegisterProvider("anthropic", adapters.NewAnthropicAdapter(anthropicClient))
	for _, model := range appConfig.Models {
		llmClientFactory.RegisterClientUsingConfig(model)
	}
	textHandlerFactory := services.TextServiceFactory{
		ClientFactory: llmClientFactory,
		MessagesRepo:  messagesRepo,
		UsersRepo:     userRepo,
		DialogTimeout: dialogTimeout,
	}
	voiceHandler := services.VoiceHandler{
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
	err = b.SetCommands([]tele.Command{
		{Text: "/retry", Description: "Retry the last message"},
		{Text: "/reset", Description: "Start a new dialog"},
		{Text: "/current_model", Description: "Currently selected model"},
		{Text: "/change_model", Description: "Change the model"},
		{Text: "/cancel", Description: "Cancel the current request"},
	})
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
			return c.Send("No messages found")
		}

		log.Printf("Interaction: %+v\n", interaction)
		textHandler, err := textHandlerFactory.NewTextService(user, interaction.TgUserMessageId)
		if err != nil {
			return err
		}

		var chunksCh <-chan services.Result
		if len(interaction.UserChatCompletion().MultiContent) > 0 {
			return c.Send("Cannot retry multi-content messages")
		} else {
			chunksCh = textHandler.OnStreamableTextHandler(ctx, interaction.UserChatCompletion().Content)
		}

		return telegram_utils.SendStream(c, &tele.Message{
			ID:   int(interaction.TgUserMessageId),
			Chat: c.Chat(),
		}, chunksCh)
	})

	limitedGroup.Handle("/change_model", func(c tele.Context) error {
		user := c.Get("user").(models.User)
		if len(c.Args()) == 0 {
			return c.Send("Provide model name")
		}
		modelName := c.Args()[0]
		if !llmClientFactory.IsClientRegistered(modelName) {
			return c.Send("Model not found")
		}
		user.CurrentModel = c.Args()[0]
		err := userRepo.UpdateUser(user)
		if err != nil {
			return err
		}
		return c.Send(fmt.Sprintf("Model changed to %s", c.Args()[0]))
	})

	limitedGroup.Handle("/current_model", func(c tele.Context) error {
		user := c.Get("user").(models.User)

		return c.Send(fmt.Sprintf("Current model is %s", user.CurrentModel))
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

		textHandler, err := textHandlerFactory.NewTextService(user, int64(c.Message().ID))
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

		textHandler, err := textHandlerFactory.NewTextService(user, int64(c.Message().ID))
		if err != nil {
			return err
		}
		err = c.Notify(tele.Typing)
		if err != nil {
			return err
		}
		userInput := c.Message().Text
		chunksCh := textHandler.OnStreamableTextHandler(ctx, userInput)
		return telegram_utils.SendStream(c, c.Message(), chunksCh)
	})

	limitedGroup.Handle(tele.OnPhoto, func(c tele.Context) error {
		log.Println("Got photo message")
		ctx := c.Get("requestContext").(context.Context)
		user := c.Get("user").(models.User)

		photoFile := c.Message().Photo
		reader, err := c.Bot().File(&photoFile.File)
		if err != nil {
			return err
		}
		defer reader.Close()

		fileContent, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		encodedStr := base64.StdEncoding.EncodeToString(fileContent)

		textHandler, err := textHandlerFactory.NewTextService(user, int64(c.Message().ID))
		if err != nil {
			return err
		}
		err = c.Notify(tele.Typing)
		if err != nil {
			return err
		}
		userInput := c.Message().Caption

		if len(userInput) == 0 {
			return c.Send("Provide image caption")
		}

		chunksCh := textHandler.OnStreamableVisionHandler(ctx, userInput, fmt.Sprintf("data:image/jpeg;base64,%s", encodedStr))
		return telegram_utils.SendStream(c, c.Message(), chunksCh)
	})

	log.Println("Listening...")
	b.Start()
}
