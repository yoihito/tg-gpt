package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

type summaryInputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func summaryEvent(t *testing.T, eventType string, payload any) models.TraceEvent {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return models.TraceEvent{EventType: eventType, Payload: data}
}

func TestSummarizerSendsOriginalImages(t *testing.T) {
	image1 := "data:image/jpeg;base64,aW1hZ2Ux"
	image2 := "data:image/png;base64,aW1hZ2Uy"
	text := func(value string) summaryInputPart { return summaryInputPart{Type: "input_text", Text: value} }
	image := func(url string) summaryInputPart {
		return summaryInputPart{Type: "input_image", ImageURL: url, Detail: "auto"}
	}
	cases := []struct {
		name   string
		events []models.TraceEvent
		want   []summaryInputPart
	}{
		{name: "text conversation", events: []models.TraceEvent{
			summaryEvent(t, models.EventTypeUserMsg, models.UserMsgPayload{Content: "Describe this"}),
			summaryEvent(t, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "A description"}),
		}, want: []summaryInputPart{text("User:\n"), text("Describe this"), text("Assistant: A description\n")}},
		{name: "album preserves images and text order", events: []models.TraceEvent{
			summaryEvent(t, models.EventTypeUserMsg, models.UserMsgPayload{MultiContent: []llm.ContentPart{
				{Type: llm.ContentPartText, Text: "Compare these"},
				{Type: llm.ContentPartImageURL, ImageURL: image1},
				{Type: llm.ContentPartText, Text: "Second image"},
				{Type: llm.ContentPartImageURL, ImageURL: image2},
			}}),
			summaryEvent(t, models.EventTypeToolResult, models.ToolResultPayload{Name: "search", Result: "Found context"}),
			summaryEvent(t, models.EventTypeModelMsg, models.ModelMsgPayload{Content: "They differ"}),
		}, want: []summaryInputPart{text("User:\n"), text("Compare these"), image(image1), text("Second image"), image(image2), text("Tool search: Found context\n"), text("Assistant: They differ\n")}},
		{name: "image without caption", events: []models.TraceEvent{
			summaryEvent(t, models.EventTypeUserMsg, models.UserMsgPayload{MultiContent: []llm.ContentPart{{Type: llm.ContentPartImageURL, ImageURL: image1}}}),
		}, want: []summaryInputPart{text("User:\n"), image(image1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var request struct {
				Model        string `json:"model"`
				Instructions string `json:"instructions"`
				Input        []struct {
					Role    string             `json:"role"`
					Content []summaryInputPart `json:"content"`
				} `json:"input"`
			}
			received := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- json.NewDecoder(r.Body).Decode(&request)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_summary","object":"response","status":"completed","output":[{"id":"msg_summary","type":"message","role":"assistant","content":[{"type":"output_text","text":"  Summary of the dialog.  "}]}]}`))
			}))
			defer server.Close()
			client := openai.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/v1"))
			summary, err := NewSummarizer(&client, "test-vision-model").Summarize(context.Background(), tc.events)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-received; err != nil {
				t.Fatal(err)
			}
			if summary != "Summary of the dialog." {
				t.Fatalf("unexpected summary: %q", summary)
			}
			if request.Model != "test-vision-model" || request.Instructions != summarizerSystemPrompt {
				t.Fatal("summarizer configuration lost")
			}
			if len(request.Input) != 1 || request.Input[0].Role != "user" {
				t.Fatalf("unexpected transcript input: %#v", request.Input)
			}
			if !reflect.DeepEqual(request.Input[0].Content, tc.want) {
				t.Fatalf("got %#v, want %#v", request.Input[0].Content, tc.want)
			}
		})
	}
}

func TestSummarizerSkipsEmptyTranscript(t *testing.T) {
	// A nil client ensures empty/invalid traces return before making an API request.
	summarizer := NewSummarizer(nil, "test")
	for _, events := range [][]models.TraceEvent{
		nil,
		{{EventType: models.EventTypeUserMsg, Payload: json.RawMessage(`{`)}},
		{summaryEvent(t, models.EventTypeUserMsg, models.UserMsgPayload{MultiContent: []llm.ContentPart{
			{Type: llm.ContentPartText, Text: "  "}, {Type: llm.ContentPartImageURL, ImageURL: ""},
		}})},
	} {
		summary, err := summarizer.Summarize(context.Background(), events)
		if err != nil || summary != "" {
			t.Fatalf("got %q, %v", summary, err)
		}
	}
}
