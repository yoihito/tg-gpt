package tgbot

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/llm"
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
	turnDispatcher *services.TurnDispatcher,
	userRepo *repositories.UserRepo,
	traceStore *services.TraceStore,
	episodeStore *services.EpisodeStore,
	llmClientProxy *services.LLMClientProxy,
) {
	handler := NewBotHandler(
		rateLimiter,
		textService,
		voiceService,
		turnDispatcher,
		userRepo,
		traceStore,
		episodeStore,
		llmClientProxy,
	)

	bot.Handle("/cancel", func(c tele.Context) error {
		user := c.Get("user").(models.User)
		rateLimiter.CancelRequest(user)
		dialogID := int64(c.Message().ThreadID)
		return turnDispatcher.CancelDialog(c.Get("requestContext").(context.Context), user.Id, dialogID)
	})

	protected := bot.Group()
	protected.Handle("/start", func(c tele.Context) error {
		return c.Send("Hello! I'm a bot that can talk to you. Just send me a voice message or text and I will respond to you.")
	})
	protected.Handle("/retry", handler.RetryLastMessage)
	protected.Handle("/change_model", handler.ListModels)
	protected.Handle("/current_model", handler.GetCurrentModel)
	protected.Handle(tele.OnVoice, handler.HandleVoice)
	protected.Handle(tele.OnText, handler.HandleText)
	protected.Handle(tele.OnPhoto, handler.HandlePhoto)
	protected.Handle(tele.OnTopicClosed, handler.OnTopicClosed)
	protected.Handle(&tele.Btn{Unique: "model"}, handler.ChangeModel)
}

type BotHandler struct {
	rateLimiter    *middleware.RateLimiter
	textService    *services.TextService
	voiceService   *services.VoiceService
	dispatcher     *services.TurnDispatcher
	userRepo       *repositories.UserRepo
	traceStore     *services.TraceStore
	episodeStore   *services.EpisodeStore
	llmClientProxy *services.LLMClientProxy
}

func NewBotHandler(
	rateLimiter *middleware.RateLimiter,
	textService *services.TextService,
	voiceService *services.VoiceService,
	turnDispatcher *services.TurnDispatcher,
	userRepo *repositories.UserRepo,
	traceStore *services.TraceStore,
	episodeStore *services.EpisodeStore,
	llmClientProxy *services.LLMClientProxy,
) *BotHandler {
	return &BotHandler{
		rateLimiter:    rateLimiter,
		textService:    textService,
		voiceService:   voiceService,
		dispatcher:     turnDispatcher,
		userRepo:       userRepo,
		traceStore:     traceStore,
		episodeStore:   episodeStore,
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
	streamer := telegram_utils.NewTelegramStreamer(c, c.Message())

	err = h.dispatcher.Submit(
		ctx,
		user,
		int64(c.Message().ThreadID),
		int64(c.Message().ID),
		llmUserMessage(userInput),
		streamer,
	)
	if err != nil {
		slog.ErrorContext(ctx, "Error submitting text", "error", err)
		c.Send("Failed to answer the message")
		return err
	}
	return nil
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

	streamer := telegram_utils.NewTelegramStreamer(c, c.Message())

	return h.dispatcher.Submit(
		ctx,
		user,
		int64(c.Message().ThreadID),
		int64(c.Message().ID),
		llmUserMessage(transcriptionText),
		streamer,
	)
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

	streamer := telegram_utils.NewTelegramStreamer(c, c.Message())
	return h.dispatcher.Submit(
		ctx,
		user,
		int64(c.Message().ThreadID),
		int64(c.Message().ID),
		llmVisionMessage(userInput, fmt.Sprintf("data:image/jpeg;base64,%s", encodedStr)),
		streamer,
	)
}

func (h *BotHandler) RetryLastMessage(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	slog.DebugContext(ctx, "Retrying last message")
	if err := c.Notify(tele.Typing); err != nil {
		return err
	}

	user := c.Get("user").(models.User)
	dialogID := int64(c.Message().ThreadID)
	if h.dispatcher.IsActive(user.Id, dialogID) {
		return c.Send("Cannot retry while a response is being generated. Use /cancel first.")
	}
	userMsg, tgMsgID, err := h.traceStore.PopForRetry(user.Id, dialogID)
	if err != nil {
		return c.Send("No messages found")
	}

	if len(userMsg.Parts) > 0 {
		return c.Send("Cannot retry multi-content messages")
	}

	streamer := telegram_utils.NewTelegramStreamer(c, &tele.Message{
		ID:       int(tgMsgID),
		Chat:     c.Chat(),
		ThreadID: c.Message().ThreadID,
	})
	return h.textService.RetryWithMessage(ctx, user, dialogID, tgMsgID, userMsg, streamer)
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
	if err := h.userRepo.SetCurrentModel(user.Id, modelName); err != nil {
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

// OnTopicClosed fires when the user closes a forum topic in their private chat with the
// bot. A closed topic is the Telegram-native signal that a dialog is "done", so this is
// what now triggers episodic summarization (previously done by /new_chat and the dialog
// timeout, both removed now that dialog identity is the topic's thread ID rather than an
// app-level counter).
func (h *BotHandler) OnTopicClosed(c tele.Context) error {
	ctx := c.Get("requestContext").(context.Context)
	user := c.Get("user").(models.User)
	dialogID := int64(c.Message().ThreadID)
	slog.DebugContext(ctx, "Topic closed, summarizing dialog", "user_id", user.Id, "dialog_id", dialogID)
	go h.episodeStore.CloseDialog(context.WithoutCancel(ctx), user.Id, dialogID)
	return nil
}

func llmUserMessage(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: text}
}

func llmVisionMessage(text string, imageURL string) llm.Message {
	return llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.ContentPart{
			{Type: llm.ContentPartText, Text: text},
			{Type: llm.ContentPartImageURL, ImageURL: imageURL},
		},
	}
}
