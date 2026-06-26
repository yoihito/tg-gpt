package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"vadimgribanov.com/tg-gpt/internal/llm"
)

func TestClampMaxResults(t *testing.T) {
	if got := clampMaxResults(0); got != defaultWebSearchMaxResults {
		t.Fatalf("default max results: got %d", got)
	}
	if got := clampMaxResults(99); got != hardWebSearchMaxResults {
		t.Fatalf("hard max results: got %d", got)
	}
	if got := clampMaxResults(3); got != 3 {
		t.Fatalf("explicit max results: got %d", got)
	}
}

func TestTavilyTimeRange(t *testing.T) {
	cases := map[int]string{
		0:   "",
		1:   "day",
		7:   "week",
		30:  "month",
		365: "year",
		999: "",
	}
	for days, want := range cases {
		if got := tavilyTimeRange(days); got != want {
			t.Fatalf("days %d: got %q want %q", days, got, want)
		}
	}
}

func TestCleanDomains(t *testing.T) {
	got := cleanDomains([]string{" https://example.com/docs/ ", "http://openai.com", "", "platform.openai.com/"})
	want := []string{"example.com/docs", "openai.com", "platform.openai.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestFormatTavilyResponseBoundsContent(t *testing.T) {
	longContent := strings.Repeat("x", maxWebSearchResultContentLen+100)
	out := formatTavilyResponse(webSearchArgs{
		Query:          "test query",
		IncludeContent: true,
	}, tavilySearchResponse{
		Results: []tavilySearchResult{
			{
				Title:         "Result",
				URL:           "https://example.com",
				Content:       "Snippet",
				RawContent:    longContent,
				PublishedDate: "2026-06-25",
				Score:         0.9,
			},
		},
	})

	var parsed struct {
		Query   string `json:"query"`
		Notice  string `json:"notice"`
		Results []struct {
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if parsed.Query != "test query" {
		t.Fatalf("query: got %q", parsed.Query)
	}
	if parsed.Notice == "" {
		t.Fatal("expected untrusted-content notice")
	}
	if len(parsed.Results) != 1 {
		t.Fatalf("results len: got %d", len(parsed.Results))
	}
	if !strings.Contains(parsed.Results[0].Content, "[truncated]") {
		t.Fatalf("expected truncated content, got %q", parsed.Results[0].Content)
	}
}

func TestWebSearchEmptyTavilyResponseReturnsToolResult(t *testing.T) {
	service := NewWebSearchService("test-key")
	service.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})}

	result, err := service.HandleToolCall(context.Background(), llm.ToolCall{
		Name:      "web_search",
		Arguments: `{"query":"test"}`,
	})
	if err != nil {
		t.Fatalf("expected tool-level failure result, got error: %v", err)
	}

	var parsed struct {
		Query string `json:"query"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, result)
	}
	if parsed.Query != "test" {
		t.Fatalf("query: got %q", parsed.Query)
	}
	if !strings.Contains(parsed.Error, "empty response") {
		t.Fatalf("error: got %q", parsed.Error)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
