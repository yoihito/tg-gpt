package services

import (
	"encoding/json"
	"fmt"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

// TraceStore is the per-turn conversation journal: it appends user/model/tool events to
// the trace and lets a turn be popped back off for retry. It owns no retrieval or
// consolidation logic — see Retriever and MemoryConsolidator for those.
type TraceStore struct {
	trace *repositories.TraceRepo
}

func NewTraceStore(trace *repositories.TraceRepo) *TraceStore {
	return &TraceStore{trace: trace}
}

type TurnContext struct {
	UserID      int64
	DialogID    int64
	UserTraceID int64
}

// BeginTurn writes the user_msg trace event and returns a TurnContext that subsequent
// calls thread through. The returned UserTraceID is the source_trace_id for any
// candidates promoted from this turn.
func (t *TraceStore) BeginTurn(
	userID, dialogID int64,
	msg llm.Message,
	tgMsgID int64,
) (TurnContext, error) {
	payload := models.UserMsgPayload{
		Content:      msg.Content,
		MultiContent: msg.Parts,
	}
	var tgPtr *int64
	if tgMsgID != 0 {
		tgPtr = &tgMsgID
	}
	id, err := t.trace.Append(repositories.AppendEventInput{
		UserID:      userID,
		DialogID:    dialogID,
		EventType:   models.EventTypeUserMsg,
		Payload:     payload,
		TgMessageID: tgPtr,
	})
	if err != nil {
		return TurnContext{}, fmt.Errorf("append user_msg: %w", err)
	}
	return TurnContext{UserID: userID, DialogID: dialogID, UserTraceID: id}, nil
}

// PopForRetry deletes the most recent user_msg event and everything after it in the
// current dialog, returning the popped user message so it can be replayed.
func (t *TraceStore) PopForRetry(userID, dialogID int64) (llm.Message, int64, error) {
	e, err := t.trace.PopLatestExchange(userID, dialogID)
	if err != nil {
		return llm.Message{}, 0, err
	}
	var p models.UserMsgPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return llm.Message{}, 0, fmt.Errorf("parse user_msg payload: %w", err)
	}
	var tgMsgID int64
	if e.TgMessageID != nil {
		tgMsgID = *e.TgMessageID
	}
	msg := llm.Message{Role: llm.RoleUser}
	if len(p.MultiContent) > 0 {
		msg.Parts = p.MultiContent
	} else {
		msg.Content = p.Content
	}
	return msg, tgMsgID, nil
}

// AppendModelMsg records the assistant's response (with any tool calls) as a model_msg event.
func (t *TraceStore) AppendModelMsg(
	mctx TurnContext,
	content string,
	toolCalls []llm.ToolCall,
	model string,
	tgMsgID int64,
) (int64, error) {
	payload := models.ModelMsgPayload{
		Content:   content,
		ToolCalls: toolCalls,
	}
	var tgPtr *int64
	if tgMsgID != 0 {
		tgPtr = &tgMsgID
	}
	return t.trace.Append(repositories.AppendEventInput{
		UserID:      mctx.UserID,
		DialogID:    mctx.DialogID,
		EventType:   models.EventTypeModelMsg,
		Payload:     payload,
		TgMessageID: tgPtr,
		Model:       model,
	})
}

// AppendToolResult records a tool's response.
func (t *TraceStore) AppendToolResult(
	mctx TurnContext,
	toolCallID, name, result string,
) (int64, error) {
	payload := models.ToolResultPayload{
		ToolCallID: toolCallID,
		Name:       name,
		Result:     result,
	}
	return t.trace.Append(repositories.AppendEventInput{
		UserID:    mctx.UserID,
		DialogID:  mctx.DialogID,
		EventType: models.EventTypeToolResult,
		Payload:   payload,
	})
}

// RecordReminderFire records a fired reminder as a synthetic user/model exchange in
// the trace so subsequent conversation has context that the reminder went off.
func (t *TraceStore) RecordReminderFire(
	userID, dialogID int64,
	userText, assistantText string,
	tgAssistantMsgID int64,
) error {
	_, err := t.trace.Append(repositories.AppendEventInput{
		UserID:    userID,
		DialogID:  dialogID,
		EventType: models.EventTypeUserMsg,
		Payload:   models.UserMsgPayload{Content: userText},
	})
	if err != nil {
		return err
	}
	var tgPtr *int64
	if tgAssistantMsgID != 0 {
		tgPtr = &tgAssistantMsgID
	}
	_, err = t.trace.Append(repositories.AppendEventInput{
		UserID:      userID,
		DialogID:    dialogID,
		EventType:   models.EventTypeModelMsg,
		Payload:     models.ModelMsgPayload{Content: assistantText},
		TgMessageID: tgPtr,
	})
	return err
}
