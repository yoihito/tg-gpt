package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/config"
	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/delivery/tgbot"
	"vadimgribanov.com/tg-gpt/internal/middleware"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/services"
	"vadimgribanov.com/tg-gpt/pkg/logging"
)

func main() {
	ctx := context.Background()
	if err := logging.SetupLogger(ctx); err != nil {
		slog.ErrorContext(ctx, "Error setting up logger", "error", err)
		return
	}

	if err := godotenv.Load(); err != nil {
		slog.ErrorContext(ctx, "Error loading .env file", "error", err)
	}

	appConfig, err := config.LoadConfig()
	if err != nil {
		slog.ErrorContext(ctx, "Error loading config", "error", err)
		return
	}

	// Initialize database
	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "data/tg-gpt.db"
	}

	db, err := database.NewDB(dbPath)
	if err != nil {
		slog.ErrorContext(ctx, "Error initializing database", "error", err)
		return
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		slog.ErrorContext(ctx, "Error running database migrations", "error", err)
		return
	}

	messagesRepo := repositories.NewMessagesRepo(db)
	memoryRepo := repositories.NewMemoryRepo(db)

	allowedUserIDsStr := os.Getenv("ALLOWED_USER_ID")
	allowedUserIDs := make([]int64, 0)
	for _, idStr := range strings.Split(allowedUserIDsStr, ",") {
		id, err := strconv.ParseInt(idStr, 10, 0)
		if err != nil {
			slog.ErrorContext(ctx, "Error parsing allowed user ID", "error", err)
			return
		}
		allowedUserIDs = append(allowedUserIDs, id)
	}
	userRepo := repositories.NewUserRepo(db)
	dialogTimeout := int64(appConfig.DialogTimeout)
	maxConcurrentRequests := appConfig.MaxConcurrentRequests
	rateLimiter := middleware.RateLimiter{MaxConcurrentRequests: maxConcurrentRequests}
	authenticator := middleware.UserAuthenticator{UserRepo: userRepo, AllowedUserIds: allowedUserIDs, AppConfig: *appConfig}
	llmClientProxy := services.NewClientProxyFromConfig(appConfig)
	memoryService := services.NewMemoryService(memoryRepo)
	textService := services.NewTextService(
		llmClientProxy,
		messagesRepo,
		userRepo,
		memoryService,
		dialogTimeout,
		appConfig.DefaultModel.ModelId,
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
		slog.ErrorContext(ctx, "Error creating bot", "error", err)
		return
	}
	// b.Use(tele_middleware.Recover(func(err error) {
	// 	slog.ErrorContext(ctx, "Error in middleware", "error", err)
	// }))
	b.Use(func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			newCtx := context.WithValue(ctx, "tg_user_id", c.Sender().ID)
			requestID := uuid.New().String()
			newCtx = context.WithValue(newCtx, "request_id", requestID)
			c.Set("requestContext", newCtx)
			return next(c)
		}
	})
	b.Use(middleware.Logger())
	b.Use(authenticator.Middleware())

	err = b.SetCommands([]tele.Command{
		{Text: "/retry", Description: "Retry the last message"},
		{Text: "/new_chat", Description: "Start a new dialog"},
		{Text: "/current_model", Description: "Currently selected model"},
		{Text: "/change_model", Description: "Change the model"},
		{Text: "/cancel", Description: "Cancel the current request"},
	})
	if err != nil {
		slog.ErrorContext(ctx, "Error setting commands", "error", err)
		return
	}

	tgbot.RegisterHandlers(
		b,
		&rateLimiter,
		textService,
		voiceService,
		userRepo,
		messagesRepo,
		llmClientProxy,
	)

	slog.InfoContext(ctx, "Listening...")
	b.Start()
}
