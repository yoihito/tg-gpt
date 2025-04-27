package tgbot

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/middleware"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/services"
	"vadimgribanov.com/tg-gpt/internal/telegram_utils"
)

func RegisterHandlers(
	bot *tele.Bot,
	rateLimiter *middleware.RateLimiter,
	textService *services.TextService,
	voiceService *services.VoiceService,
	userRepo *repositories.UserRepo,
	messagesRepo *repositories.MessagesRepo,
	llmClientProxy *services.LLMClientProxy,
) {
	handler := NewBotHandler(
		rateLimiter,
		textService,
		voiceService,
		userRepo,
		messagesRepo,
		llmClientProxy,
	)

	bot.Handle("/cancel", func(c tele.Context) error {
		rateLimiter.CancelRequest(c.Get("user").(models.User))
		return nil
	})

	protected := bot.Group()
	protected.Use(rateLimiter.Middleware())
	protected.Handle("/start", func(c tele.Context) error {
		return c.Send("Hello! I'm a bot that can talk to you. Just send me a voice message or text and I will respond to you.")
	})
	protected.Handle("/new_chat", handler.NewDialog)
	protected.Handle("/retry", handler.RetryLastMessage)
	protected.Handle("/change_model", handler.ListModels)
	protected.Handle("/current_model", handler.GetCurrentModel)
	protected.Handle(tele.OnVoice, handler.HandleVoice)
	protected.Handle(tele.OnText, handler.HandleText)
	protected.Handle(tele.OnPhoto, handler.HandlePhoto)
	protected.Handle(&tele.Btn{Unique: "model"}, handler.ChangeModel)
}

type BotHandler struct {
	rateLimiter    *middleware.RateLimiter
	textService    *services.TextService
	voiceService   *services.VoiceService
	userRepo       *repositories.UserRepo
	messagesRepo   *repositories.MessagesRepo
	llmClientProxy *services.LLMClientProxy
}

func NewBotHandler(
	rateLimiter *middleware.RateLimiter,
	textService *services.TextService,
	voiceService *services.VoiceService,
	userRepo *repositories.UserRepo,
	messagesRepo *repositories.MessagesRepo,
	llmClientProxy *services.LLMClientProxy,
) *BotHandler {
	return &BotHandler{
		rateLimiter:    rateLimiter,
		textService:    textService,
		voiceService:   voiceService,
		userRepo:       userRepo,
		messagesRepo:   messagesRepo,
		llmClientProxy: llmClientProxy,
	}
}

func (h *BotHandler) HandleText(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Got text message")
	user := c.Get("user").(models.User)

	err := c.Notify(tele.Typing)
	if err != nil {
		return err
	}
	userInput := c.Message().Text
	chunksCh := h.textService.OnStreamableTextHandler(ctx, user, int64(c.Message().ID), userInput)
	return telegram_utils.SendStream(c, c.Message(), chunksCh)
}

func (h *BotHandler) HandleVoice(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Got voice message")
	voiceFile := c.Message().Voice

	reader, err := c.Bot().File(&voiceFile.File)
	if err != nil {
		return err
	}
	defer reader.Close()

	user := c.Get("user").(models.User)
	transcriptionText, err := h.voiceService.OnVoiceHandler(ctx, reader)
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

	chunksCh := h.textService.OnStreamableTextHandler(
		ctx,
		user,
		int64(c.Message().ID),
		transcriptionText,
	)

	return telegram_utils.SendStream(c, c.Message(), chunksCh)
}

func (h *BotHandler) HandlePhoto(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Got photo message")
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

	err = c.Notify(tele.Typing)
	if err != nil {
		return err
	}
	userInput := c.Message().Caption

	if len(userInput) == 0 {
		return c.Send("Provide image caption")
	}

	chunksCh := h.textService.OnStreamableVisionHandler(
		ctx,
		user,
		int64(c.Message().ID),
		userInput,
		fmt.Sprintf("data:image/jpeg;base64,%s", encodedStr),
	)
	return telegram_utils.SendStream(c, c.Message(), chunksCh)
}

func (h *BotHandler) RetryLastMessage(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Retrying last message")
	err := c.Notify(tele.Typing)
	if err != nil {
		return err
	}

	user := c.Get("user").(models.User)
	interaction, err := h.messagesRepo.PopLatestInteraction(user)
	if err != nil {
		return c.Send("No messages found")
	}

	slog.DebugContext(ctx, "Last interaction with the user", "interaction", interaction)

	var chunksCh <-chan services.Result
	if len(interaction.UserMessage.MultiContent) > 0 {
		return c.Send("Cannot retry multi-content messages")
	} else {
		chunksCh = h.textService.RetryInteraction(ctx, user, interaction.TgUserMessageId, interaction)
	}

	return telegram_utils.SendStream(c, &tele.Message{
		ID:   int(interaction.TgUserMessageId),
		Chat: c.Chat(),
	}, chunksCh)
}

func (h *BotHandler) ListModels(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Changing model")

	models := h.llmClientProxy.ListModels()
	slog.InfoContext(ctx, "Models", "models", models)
	selector := &tele.ReplyMarkup{}
	rows := make([]tele.Row, 0, len(models))
	for _, model := range models {
		btn := selector.Data(model, "model", model)
		rows = append(rows, selector.Row(btn))
	}
	selector.Inline(rows...)

	return c.Send("Choose model", selector)
}

func (h *BotHandler) ChangeModel(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Changing model")

	user := c.Get("user").(models.User)

	modelName := c.Args()[0]
	if !h.llmClientProxy.IsClientRegistered(modelName) {
		return c.Send("Model not found")
	}
	user.CurrentModel = modelName
	err := h.userRepo.UpdateUser(user)
	if err != nil {
		return err
	}
	return c.Send(fmt.Sprintf("Model changed to %s", modelName))
}

func (h *BotHandler) GetCurrentModel(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Getting current model")
	user := c.Get("user").(models.User)
	return c.Send(fmt.Sprintf("Current model is %s", user.CurrentModel))
}

func (h *BotHandler) NewDialog(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Starting new dialog")

	user := c.Get("user").(models.User)
	user.StartNewDialog()
	err := h.userRepo.UpdateUser(user)
	if err != nil {
		return err
	}
	return c.Send("New dialog started")

}
