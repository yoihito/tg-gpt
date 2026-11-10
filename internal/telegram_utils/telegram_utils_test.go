package telegram_utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	tele "gopkg.in/telebot.v3"
	"vadimgribanov.com/tg-gpt/internal/llm"
)

func testStreamer(t *testing.T, handler http.HandlerFunc) *TelegramStreamer {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: server.URL, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	message := &tele.Message{ID: 1, Chat: &tele.Chat{ID: 10}, ThreadID: 42}
	c := bot.NewContext(tele.Update{Message: message})
	c.Set("requestContext", context.Background())
	return NewTelegramStreamer(c, message)
}

func telegramOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true,"result":{"message_id":2,"chat":{"id":10}}}`)
}

func TestStreamingCoalescesAndFlushesFinalText(t *testing.T) {
	calls := 0
	var texts []string
	streamer := testStreamer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Text     string `json:"text"`
			ThreadID string `json:"message_thread_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		texts = append(texts, body.Text)
		telegramOK(w)
	})
	for i := 0; i < 10; i++ {
		if err := streamer.SendEvent(llm.StreamEvent{TextDelta: strings.Repeat("x", 200)}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("burst made %d requests, want 1", calls)
	}
	// Advance past the cooldown without slowing the test.
	streamer.delivery.nextCall = time.Time{}
	if err := streamer.Flush(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || texts[1] != strings.Repeat("x", 2000) {
		t.Fatal("final text lost")
	}
	if err := streamer.Flush(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("unchanged content was sent again")
	}
}

func TestFloodDefersWithoutFallbackAndRetriesFinal(t *testing.T) {
	calls := 0
	var sentText string
	streamer := testStreamer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 230","parameters":{"retry_after":230}}`)
			return
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		sentText = body.Text
		telegramOK(w)
	})
	if err := streamer.SendEvent(llm.StreamEvent{TextDelta: strings.Repeat("a", 200)}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("flood triggered fallback requests")
	}
	if time.Until(streamer.delivery.nextCall) < 229*time.Second {
		t.Fatal("server retry_after not respected")
	}
	if err := streamer.SendEvent(llm.StreamEvent{TextDelta: " more"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("sent during flood cooldown")
	}
	streamer.delivery.nextCall = time.Time{}
	if err := streamer.Flush(); err != nil {
		t.Fatal(err)
	}
	if sentText != strings.Repeat("a", 200)+" more" {
		t.Fatal("deferred response lost")
	}
}

func TestFloodWaitIsCancellable(t *testing.T) {
	streamer := testStreamer(t, func(w http.ResponseWriter, r *http.Request) { telegramOK(w) })
	streamer.accumulatedMessage = "pending answer"
	streamer.delivery.nextCall = time.Now().Add(230 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	streamer.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- streamer.Flush() }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt delivery")
	}
}

func TestFinalFloodRetries(t *testing.T) {
	delivery := &chatDelivery{gate: make(chan struct{}, 1)}
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	_, sent, err := delivery.deliver(ctx, true, func() (*tele.Message, error) {
		calls++
		if calls == 1 {
			return nil, tele.FloodError{RetryAfter: 1}
		}
		return &tele.Message{ID: 1}, nil
	})
	if err != nil || !sent || calls != 2 {
		t.Fatalf("sent=%v calls=%d err=%v", sent, calls, err)
	}
	if time.Since(started) < telegramUpdateInterval {
		t.Fatal("retried before cooldown elapsed")
	}
}

func TestSharedChatCooldown(t *testing.T) {
	bot := &tele.Bot{}
	first := deliveryFor(bot, "10")
	if deliveryFor(bot, "10") != first {
		t.Fatal("cooldown not shared between topics")
	}
	if deliveryFor(bot, "11") == first {
		t.Fatal("unrelated chats share cooldown")
	}
	first.nextCall = time.Now().Add(time.Second)
	_, sent, err := first.deliver(context.Background(), false, func() (*tele.Message, error) { t.Fatal("sent during cooldown"); return nil, nil })
	if err != nil || sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
}

func TestLongUnicodeDeltaIsPreserved(t *testing.T) {
	var texts []string
	streamer := testStreamer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		texts = append(texts, body.Text)
		telegramOK(w)
	})
	text := strings.Repeat("Привет🙂", 700)
	if err := streamer.SendEvent(llm.StreamEvent{TextDelta: text}); err != nil {
		t.Fatal(err)
	}
	streamer.delivery.nextCall = time.Time{}
	if err := streamer.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(texts, "") != text {
		t.Fatal("long delta truncated or duplicated")
	}
	for _, part := range texts {
		if !utf8.ValidString(part) || len(utf16.Encode([]rune(part))) > MaxTelegramMessageLength {
			t.Fatal("invalid Unicode chunk")
		}
	}
}

func TestBusyChatSkipsIntermediateAndCancelsRequiredDelivery(t *testing.T) {
	delivery := &chatDelivery{gate: make(chan struct{}, 1)}
	delivery.gate <- struct{}{}
	defer func() { <-delivery.gate }()
	operation := func() (*tele.Message, error) { t.Error("operation ran while chat was busy"); return nil, nil }
	_, sent, err := delivery.deliver(context.Background(), false, operation)
	if sent || err != nil {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := delivery.deliver(ctx, true, operation); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt lock acquisition")
	}
}

func TestMarkdownFallbackOnlyRetriesFormattingErrors(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		calls int
	}{
		{"formatting", tele.NewError(400, "Bad Request: can't parse entities"), 2},
		{"flood", tele.FloodError{RetryAfter: 230}, 1},
		{"forbidden", tele.NewError(403, "Forbidden: bot was blocked by the user"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := withMarkdownFallback("*unfinished", func(text string, mode tele.ParseMode) (*tele.Message, error) {
				calls++
				if calls == 1 {
					if mode != tele.ModeMarkdown || text != "*unfinished*" {
						t.Fatal("incorrect Markdown attempt")
					}
					return nil, tc.err
				}
				if mode != tele.ModeDefault || text != "*unfinished" {
					t.Fatal("incorrect plain-text fallback")
				}
				return &tele.Message{ID: 1}, nil
			})
			if calls != tc.calls {
				t.Fatalf("got %d calls, want %d", calls, tc.calls)
			}
			if tc.calls == 2 && err != nil {
				t.Fatal(err)
			}
			if tc.calls == 1 && err != tc.err {
				t.Fatal("original error was lost")
			}
		})
	}
}

func TestTrimIncompleteLink(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"no link", "just plain text", "just plain text"},
		{"completed link", "see [docs](https://example.com) for more", "see [docs](https://example.com) for more"},
		{"open bracket only", "check this [", "check this "},
		{"link text not yet closed", "check this [docs", "check this "},
		{"closed bracket awaiting paren", "check this [docs]", "check this "},
		{"paren opened, url truncated", "check this [docs](https://exa", "check this "},
		{"bracket not a link, followed by space", "a [note] on the side continues", "a [note] on the side continues"},
		{"escaped bracket", `an escaped \[ bracket`, `an escaped \[ bracket`},
		{"two links, second incomplete", "[a](https://a.com) and [b](https://b", "[a](https://a.com) and "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimIncompleteLink(tc.text); got != tc.want {
				t.Fatalf("trimIncompleteLink(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestStreamingHoldsBackIncompleteLinkUntilClosed(t *testing.T) {
	var texts []string
	streamer := testStreamer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		texts = append(texts, body.Text)
		telegramOK(w)
	})
	// Enough text to cross the streaming interval, ending mid-link.
	if err := streamer.SendEvent(llm.StreamEvent{TextDelta: strings.Repeat("x", 200) + " see [docs](https://exa"}); err != nil {
		t.Fatal(err)
	}
	if len(texts) != 1 || strings.Contains(texts[0], "[docs]") {
		t.Fatalf("sent an incomplete link intermediately: %v", texts)
	}
	streamer.delivery.nextCall = time.Time{}
	if err := streamer.SendEvent(llm.StreamEvent{TextDelta: "mple.com) now closed " + strings.Repeat("y", 200)}); err != nil {
		t.Fatal(err)
	}
	if len(texts) != 2 || !strings.Contains(texts[1], "[docs](https://example.com)") {
		t.Fatalf("completed link was not sent once closed: %v", texts)
	}
	streamer.delivery.nextCall = time.Time{}
	if err := streamer.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(texts[len(texts)-1], "now closed") {
		t.Fatal("final flush lost trailing content")
	}
}

func TestFinalFlushSendsStillIncompleteLink(t *testing.T) {
	var texts []string
	streamer := testStreamer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		texts = append(texts, body.Text)
		telegramOK(w)
	})
	streamer.accumulatedMessage = "the model stopped mid [link](https://exa"
	if err := streamer.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(texts) != 1 || texts[0] != "the model stopped mid [link](https://exa" {
		t.Fatalf("final flush should send everything as-is: %v", texts)
	}
}

func TestSendChunkedHonorsCancellation(t *testing.T) {
	streamer := testStreamer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("sent after cancellation"); telegramOK(w) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := SendChunked(ctx, streamer.bot, &tele.Chat{ID: 10}, "scheduled answer")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestMarkdownClosingDelimiterDoesNotFailUnchangedEdit(t *testing.T) {
	for _, description := range []string{
		"Bad Request: message is not modified",
		"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message",
	} {
		t.Run(description, func(t *testing.T) {
			var requests []string
			streamer := testStreamer(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Text string `json:"text"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests = append(requests, body.Text)
				if strings.HasSuffix(r.URL.Path, "/editMessageText") {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": description})
					return
				}
				telegramOK(w)
			})
			if err := streamer.SendEvent(llm.StreamEvent{TextDelta: "*" + strings.Repeat("x", 199)}); err != nil {
				t.Fatal(err)
			}
			if err := streamer.SendEvent(llm.StreamEvent{TextDelta: "*"}); err != nil {
				t.Fatal(err)
			}
			streamer.delivery.nextCall = time.Time{}
			if err := streamer.Flush(); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 || requests[0] != requests[1] {
				t.Fatalf("expected identical edit: %v", requests)
			}
			if err := streamer.Flush(); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatal("successful unchanged edit retried")
			}
		})
	}
}
