package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestNameTopicTrimsAndSendsMessage(t *testing.T) {
	var request struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Input        string `json:"input"`
	}
	received := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_topic","object":"response","status":"completed","output":[{"id":"msg_topic","type":"message","role":"assistant","content":[{"type":"output_text","text":"  \"Trip to Lisbon\"  "}]}]}`))
	}))
	defer server.Close()
	client := openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/v1"))

	name, err := NewTopicNamer(&client, "test-cheap-model").NameTopic(context.Background(), "Help me plan a trip to Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
	if name != "Trip to Lisbon" {
		t.Fatalf("expected trimmed/unquoted name, got %q", name)
	}
	if request.Model != "test-cheap-model" || request.Instructions != topicNamerSystemPrompt {
		t.Fatal("topic namer configuration lost")
	}
	if request.Input != "Help me plan a trip to Lisbon" {
		t.Fatalf("unexpected input sent to model: %q", request.Input)
	}
}

func TestNameTopicReturnsEmptyOnBlankResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_blank","object":"response","status":"completed","output":[{"id":"msg_blank","type":"message","role":"assistant","content":[{"type":"output_text","text":"   "}]}]}`))
	}))
	defer server.Close()
	client := openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/v1"))

	name, err := NewTopicNamer(&client, "test-cheap-model").NameTopic(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Fatalf("expected empty name for blank model response, got %q", name)
	}
}

func TestNameTopicTruncatesToTelegramLimit(t *testing.T) {
	long := strings.Repeat("a", telegramTopicNameLimit+50)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_long","object":"response","status":"completed","output":[{"id":"msg_long","type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}`, long)
	}))
	defer server.Close()
	client := openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/v1"))

	name, err := NewTopicNamer(&client, "test-cheap-model").NameTopic(context.Background(), "ramble")
	if err != nil {
		t.Fatal(err)
	}
	if len(name) != telegramTopicNameLimit {
		t.Fatalf("expected name truncated to %d chars, got %d", telegramTopicNameLimit, len(name))
	}
}
