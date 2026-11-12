package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/database"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
	"vadimgribanov.com/tg-gpt/internal/repositories"
)

const (
	topicPluginTestUserID   = int64(6001)
	topicPluginTestDialogID = int64(42)
	topicPluginTestChatID   = int64(777777)
)

type editTopicCall struct {
	chatID   int64
	threadID int
	name     string
}

type fakeTopicEditor struct {
	calls chan editTopicCall
}

func newFakeTopicEditor() *fakeTopicEditor {
	return &fakeTopicEditor{calls: make(chan editTopicCall, 10)}
}

func (f *fakeTopicEditor) EditTopic(chat *tele.Chat, topic *tele.Topic) error {
	f.calls <- editTopicCall{chatID: chat.ID, threadID: topic.ThreadID, name: topic.Name}
	return nil
}

func newFakeTopicNamer(t *testing.T, canned string) *TopicNamer {
	namer, _ := newFakeTopicNamerCapturing(t, canned)
	return namer
}

// newFakeTopicNamerCapturing is newFakeTopicNamer plus a channel of the raw request
// bodies the fake server received, so tests can assert on what text was actually sent
// to the model.
func newFakeTopicNamerCapturing(t *testing.T, canned string) (*TopicNamer, chan string) {
	t.Helper()
	bodies := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp","object":"response","status":"completed","output":[{"id":"msg","type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}`, canned)
	}))
	t.Cleanup(server.Close)
	client := openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/v1"))
	return NewTopicNamer(&client, "test-cheap-model"), bodies
}

func newTopicPluginTestTraceStore(t *testing.T) *TraceStore {
	t.Helper()
	db, err := database.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.NewUserRepo(db).Register(topicPluginTestUserID, "Test", "", "test", topicPluginTestChatID, true, "test-model"); err != nil {
		t.Fatal(err)
	}
	return NewTraceStore(repositories.NewTraceRepo(db))
}

func appendUserMsg(t *testing.T, repo *repositories.TraceRepo, userID, dialogID int64, text string) {
	t.Helper()
	if _, err := repo.Append(repositories.AppendEventInput{
		UserID: userID, DialogID: dialogID,
		EventType: models.EventTypeUserMsg, Payload: models.UserMsgPayload{Content: text},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTopicNamingPluginRenamesTopicOnFirstTurn(t *testing.T) {
	trace := newTopicPluginTestTraceStore(t)
	// Simulate TurnDispatcher having already attached this turn's single message.
	appendUserMsg(t, trace.trace, topicPluginTestUserID, topicPluginTestDialogID, "Help me plan a trip to Lisbon")

	namer, bodies := newFakeTopicNamerCapturing(t, "Trip to Lisbon")
	editor := newFakeTopicEditor()
	plugin := NewTopicNamingPlugin(trace, namer, editor)

	rc := &RunContext{
		User:    models.User{Id: topicPluginTestUserID, ChatId: topicPluginTestChatID},
		TurnCtx: TurnContext{UserID: topicPluginTestUserID, DialogID: topicPluginTestDialogID},
		Inputs: []UserInput{
			{Message: llm.Message{Role: llm.RoleUser, Content: "Help me plan a trip to Lisbon"}},
		},
	}
	plugin.AfterTurn(context.Background(), rc, "Sure, here's a 3-day Lisbon itinerary.")

	select {
	case call := <-editor.calls:
		if call.chatID != topicPluginTestChatID {
			t.Fatalf("expected chat id %d, got %d", topicPluginTestChatID, call.chatID)
		}
		if call.threadID != int(topicPluginTestDialogID) {
			t.Fatalf("expected thread id %d, got %d", topicPluginTestDialogID, call.threadID)
		}
		if call.name != "Trip to Lisbon" {
			t.Fatalf("expected topic name %q, got %q", "Trip to Lisbon", call.name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for EditTopic to be called")
	}

	var body string
	select {
	case body = <-bodies:
	default:
		t.Fatal("expected the namer to have been called")
	}
	if !strings.Contains(body, "Help me plan a trip to Lisbon") || !strings.Contains(body, "3-day Lisbon itinerary") {
		t.Fatalf("expected both the user message and assistant reply in the namer input, got: %s", body)
	}
}

func TestTopicNamingPluginSkipsWhenNotFirstTurn(t *testing.T) {
	trace := newTopicPluginTestTraceStore(t)
	// Two prior messages already in the dialog; this turn only attaches one more, so
	// the dialog's total (2) won't match the attached count (1) — not the first turn.
	appendUserMsg(t, trace.trace, topicPluginTestUserID, topicPluginTestDialogID, "first message")
	appendUserMsg(t, trace.trace, topicPluginTestUserID, topicPluginTestDialogID, "second message")

	namer := newFakeTopicNamer(t, "Should not be used")
	editor := newFakeTopicEditor()
	plugin := NewTopicNamingPlugin(trace, namer, editor)

	rc := &RunContext{
		User:    models.User{Id: topicPluginTestUserID, ChatId: topicPluginTestChatID},
		TurnCtx: TurnContext{UserID: topicPluginTestUserID, DialogID: topicPluginTestDialogID},
		Inputs: []UserInput{
			{Message: llm.Message{Role: llm.RoleUser, Content: "second message"}},
		},
	}
	plugin.AfterTurn(context.Background(), rc, "some reply")

	select {
	case call := <-editor.calls:
		t.Fatalf("expected no EditTopic call for a non-first turn, got %+v", call)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTopicNamingPluginSkipsGeneralTopic(t *testing.T) {
	trace := newTopicPluginTestTraceStore(t)
	namer := newFakeTopicNamer(t, "Should not be used")
	editor := newFakeTopicEditor()
	plugin := NewTopicNamingPlugin(trace, namer, editor)

	rc := &RunContext{
		User:    models.User{Id: topicPluginTestUserID, ChatId: topicPluginTestChatID},
		TurnCtx: TurnContext{UserID: topicPluginTestUserID, DialogID: 0},
		Inputs: []UserInput{
			{Message: llm.Message{Role: llm.RoleUser, Content: "hello"}},
		},
	}
	plugin.AfterTurn(context.Background(), rc, "hi there")

	select {
	case call := <-editor.calls:
		t.Fatalf("expected no EditTopic call for the General topic, got %+v", call)
	case <-time.After(100 * time.Millisecond):
	}
}
