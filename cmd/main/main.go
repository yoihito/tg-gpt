package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/config"
	"vadimgribanov.com/tg-gpt/internal/delivery/tgbot"
	"vadimgribanov.com/tg-gpt/internal/middleware"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/services"
	"vadimgribanov.com/tg-gpt/pkg/logging"
)

func main() {
	if err := logging.SetupLogger(); err != nil {
		slog.Error("Error setting up logger", "error", err)
		return
	}

	if err := godotenv.Load(); err != nil {
		slog.Error("Error loading .env file", "error", err)
	}

	appConfig, err := config.LoadConfig()
	if err != nil {
		slog.Error("Error loading config", "error", err)
	}

	messagesRepo := repositories.NewMessagesRepo()

	allowedUserIDsStr := os.Getenv("ALLOWED_USER_ID")
	allowedUserIDs := make([]int64, 0)
	for _, idStr := range strings.Split(allowedUserIDsStr, ",") {
		id, err := strconv.ParseInt(idStr, 10, 0)
		if err != nil {
			slog.Error("Error parsing allowed user ID", "error", err)
			return
		}
		allowedUserIDs = append(allowedUserIDs, id)
	}
	userRepo := repositories.NewUserRepo()
	dialogTimeout, err := strconv.ParseInt(os.Getenv("DIALOG_TIMEOUT"), 10, 0)
	if err != nil {
		slog.Error("Error parsing dialog timeout", "error", err)
		return
	}
	maxConcurrentRequests, err := strconv.Atoi(os.Getenv("MAX_CONCURRENT_REQUESTS"))
	if err != nil {
		slog.Error("Error parsing max concurrent requests", "error", err)
		return
	}
	rateLimiter := middleware.RateLimiter{MaxConcurrentRequests: maxConcurrentRequests}
	authenticator := middleware.UserAuthenticator{UserRepo: userRepo, AllowedUserIds: allowedUserIDs, AppConfig: *appConfig}
	llmClientProxy := services.NewClientProxyFromConfig(appConfig)
	textService := services.NewTextService(
		llmClientProxy,
		messagesRepo,
		userRepo,
		dialogTimeout,
	)
	voiceService := &services.VoiceService{
		Client: llmClientProxy.OpenaiClient,
	}

	pref := tele.Settings{
		Token:  os.Getenv("TOKEN"),
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		slog.Error("Error creating bot", "error", err)
		return
	}
	err = b.SetCommands([]tele.Command{
		{Text: "/retry", Description: "Retry the last message"},
		{Text: "/new_chat", Description: "Start a new dialog"},
		{Text: "/current_model", Description: "Currently selected model"},
		{Text: "/change_model", Description: "Change the model"},
		{Text: "/cancel", Description: "Cancel the current request"},
	})
	if err != nil {
		slog.Error("Error setting commands", "error", err)
		return
	}
	b.Use(middleware.Logger())
	b.Use(authenticator.Middleware())

	tgbot.RegisterHandlers(
		b,
		&rateLimiter,
		textService,
		voiceService,
		userRepo,
		messagesRepo,
		llmClientProxy,
	)

	slog.Info("Listening...")
	b.Start()
}
