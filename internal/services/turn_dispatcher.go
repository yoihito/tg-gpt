package services

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"

	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
	"vadimgribanov.com/tg-gpt/internal/telegram_utils"
)

// TurnDispatcher serializes access to each (user, dialog): at most one turn runs at a
// time per dialog, concurrent submissions for different dialogs proceed independently,
// bursts of messages that arrive before a turn starts are coalesced into one, and
// messages that arrive while a turn is already running are folded into it via
// Runner's mid-turn drain instead of racing it. It never executes a turn itself — all
// of that is delegated to TextService/Runner; this is purely the per-dialog scheduling
// layer above them (the same shape as one actor/grain per conversation, just scoped to
// this single process rather than a cluster).
// turnRunner is the seam TurnDispatcher schedules onto — satisfied by *TextService in
// production. Keeping it as an interface lets the per-dialog scheduling/coalescing
// logic be tested against a fake, without needing a full TextService (LLM client,
// memory subsystem, tool set, ...) just to drive concurrent Submit calls.
type turnRunner interface {
	PrepareUserForInput(ctx context.Context, user models.User) (models.User, error)
	RunAttachedTurn(
		ctx context.Context,
		user models.User,
		mctx TurnContext,
		inputs []UserInput,
		streamer *telegram_utils.TelegramStreamer,
		drainNewInputs func(context.Context) ([]UserInput, error),
	) (string, error)
}

type TurnDispatcher struct {
	db         *database.DB
	pending    *repositories.PendingInputRepo
	trace      *TraceStore
	text       turnRunner
	maxPending int

	mu     sync.Mutex
	active map[conversationKey]*activeConversation
}

type conversationKey struct {
	userID   int64
	dialogID int64
}

type activeConversation struct {
	cancel        context.CancelFunc
	pendingSignal bool
	streamers     map[int64]*telegram_utils.TelegramStreamer
}

func NewTurnDispatcher(
	db *database.DB,
	pending *repositories.PendingInputRepo,
	trace *TraceStore,
	text turnRunner,
) *TurnDispatcher {
	return &TurnDispatcher{
		db:         db,
		pending:    pending,
		trace:      trace,
		text:       text,
		maxPending: 100,
		active:     make(map[conversationKey]*activeConversation),
	}
}

func (r *TurnDispatcher) Submit(
	ctx context.Context,
	user models.User,
	tgMessageID int64,
	msg llm.Message,
	streamer *telegram_utils.TelegramStreamer,
) error {
	preparedUser, err := r.text.PrepareUserForInput(ctx, user)
	if err != nil {
		return err
	}
	user = preparedUser
	key := conversationKey{userID: user.Id, dialogID: user.CurrentDialogId}
	if _, err := r.pending.Insert(ctx, repositories.InsertPendingInput{
		UserID:      user.Id,
		DialogID:    user.CurrentDialogId,
		TgMessageID: tgMessageID,
		Message:     msg,
	}); err != nil {
		return err
	}

	r.mu.Lock()
	if active := r.active[key]; active != nil {
		active.pendingSignal = true
		if streamer != nil {
			active.streamers[tgMessageID] = streamer
		}
		r.mu.Unlock()
		return nil
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	active := &activeConversation{
		cancel:    cancel,
		streamers: make(map[int64]*telegram_utils.TelegramStreamer),
	}
	if streamer != nil {
		active.streamers[tgMessageID] = streamer
	}
	r.active[key] = active
	r.mu.Unlock()

	go r.run(runCtx, key, user, active)
	return nil
}

func (r *TurnDispatcher) CancelCurrentDialog(ctx context.Context, user models.User) error {
	return r.CancelDialog(ctx, user.Id, user.CurrentDialogId)
}

func (r *TurnDispatcher) CancelDialog(ctx context.Context, userID, dialogID int64) error {
	key := conversationKey{userID: userID, dialogID: dialogID}
	r.mu.Lock()
	if active := r.active[key]; active != nil {
		active.cancel()
	}
	r.mu.Unlock()
	return r.pending.DiscardForDialog(ctx, userID, dialogID)
}

func (r *TurnDispatcher) IsActive(userID, dialogID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[conversationKey{userID: userID, dialogID: dialogID}]
	return ok
}

func (r *TurnDispatcher) run(ctx context.Context, key conversationKey, user models.User, active *activeConversation) {
	defer func() {
		r.mu.Lock()
		if r.active[key] == active {
			delete(r.active, key)
		}
		r.mu.Unlock()
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		inputs, err := r.attachPendingInputs(ctx, key.userID, key.dialogID)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to attach pending inputs", "error", err, "user_id", key.userID, "dialog_id", key.dialogID)
			return
		}
		if len(inputs) == 0 {
			r.mu.Lock()
			if active.pendingSignal {
				active.pendingSignal = false
				r.mu.Unlock()
				continue
			}
			if r.active[key] == active {
				delete(r.active, key)
			}
			r.mu.Unlock()
			return
		}

		mctx := TurnContext{
			UserID:      key.userID,
			DialogID:    key.dialogID,
			UserTraceID: inputs[0].TraceID,
		}
		streamer := r.takeStreamer(active, inputs)
		_, err = r.text.RunAttachedTurn(ctx, user, mctx, inputs, streamer, func(ctx context.Context) ([]UserInput, error) {
			return r.attachPendingInputs(ctx, key.userID, key.dialogID)
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if streamer != nil {
				if noticeErr := streamer.SendStatus("Failed to answer the message"); noticeErr != nil {
					slog.ErrorContext(ctx, "Failed to send async error notice", "error", noticeErr, "user_id", key.userID, "dialog_id", key.dialogID)
				}
			}
			slog.ErrorContext(ctx, "Failed to run conversation turn", "error", err, "user_id", key.userID, "dialog_id", key.dialogID)
			return
		}
	}
}

func (r *TurnDispatcher) takeStreamer(active *activeConversation, inputs []UserInput) *telegram_utils.TelegramStreamer {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(inputs) - 1; i >= 0; i-- {
		streamer := active.streamers[inputs[i].TgMessageID]
		if streamer != nil {
			for _, input := range inputs {
				delete(active.streamers, input.TgMessageID)
			}
			return streamer
		}
	}
	return nil
}

func (r *TurnDispatcher) attachPendingInputs(ctx context.Context, userID, dialogID int64) ([]UserInput, error) {
	var attached []UserInput
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		pending, err := r.pending.ListPendingForDialogTx(tx, userID, dialogID, r.maxPending)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}

		msgs := make([]UserMsgBatchInput, 0, len(pending))
		for _, input := range pending {
			msgs = append(msgs, UserMsgBatchInput{Message: input.Message, TgMessageID: input.TgMessageID})
		}
		traceIDs, err := r.trace.AppendUserMsgBatchTx(tx, userID, dialogID, msgs)
		if err != nil {
			return err
		}
		if len(traceIDs) != len(pending) {
			return fmt.Errorf("attached trace ids mismatch: got %d want %d", len(traceIDs), len(pending))
		}
		attached = make([]UserInput, 0, len(pending))
		for i, input := range pending {
			if err := r.pending.MarkAttachedTx(tx, input.ID, traceIDs[i]); err != nil {
				return err
			}
			attached = append(attached, UserInput{
				TraceID:     traceIDs[i],
				TgMessageID: input.TgMessageID,
				Message:     input.Message,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return attached, nil
}
