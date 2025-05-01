package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{
		baseURL:    "https://api.anthropic.com",
		apiKey:     apiKey,
		httpClient: &http.Client{},
	}
}

func (c *Client) CreateMessagesStream(ctx context.Context, request CreateMessageRequest) (*StreamedResponse, error) {
	request.Stream = true
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	response, err := c.rawRequest(ctx, data)
	if err != nil {
		return nil, err
	}
	stream := NewStreamedResponse(response)
	return stream, nil
}

type CreateMessageRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Stream    bool      `json:"stream"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (c *Client) rawRequest(ctx context.Context, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/v1/messages", c.baseURL), bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp, fmt.Errorf("server returned non-200 status: %d %s", resp.StatusCode, resp.Status)
	}
	return resp, nil
}

type StreamedResponse struct {
	resp   *http.Response
	reader *bufio.Reader
}

func NewStreamedResponse(resp *http.Response) *StreamedResponse {
	return &StreamedResponse{resp: resp, reader: bufio.NewReader(resp.Body)}
}

func (s *StreamedResponse) Close() {
	s.resp.Body.Close()
}

func (s *StreamedResponse) Recv() (any, error) {
	for {
		rawLine, err := s.reader.ReadBytes('\n')
		if err != nil {
			return ContentBlockDeltaData{}, err
		}
		cleanLine := bytes.TrimSpace(rawLine)
		eventLine, ok := bytes.CutPrefix(cleanLine, []byte("event: "))
		if !ok {
			continue
		}
		data, err := s.processData(string(eventLine))
		if err != nil {
			return ContentBlockDeltaData{}, err
		}

		return data, nil
	}
}

func (s *StreamedResponse) processData(eventType string) (any, error) {
	rawLine, err := s.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}

	cleanLine := bytes.TrimSpace(rawLine)
	dataLine, ok := bytes.CutPrefix(cleanLine, []byte("data: "))
	if !ok {
		return nil, errors.New("data not found")
	}

	switch eventType {
	case "message_start":
		return unmarshalEventData[MessageStartData](dataLine)
	case "content_block_start":
		return unmarshalEventData[ContentBlockStartData](dataLine)
	case "ping":
		return unmarshalEventData[PingData](dataLine)
	case "content_block_delta":
		return unmarshalEventData[ContentBlockDeltaData](dataLine)
	case "content_block_stop":
		return unmarshalEventData[ContentBlockStopData](dataLine)
	case "message_delta":
		return unmarshalEventData[MessageDeltaData](dataLine)
	case "message_stop":
		return unmarshalEventData[MessageStopData](dataLine)
	case "error":
		return unmarshalEventData[ErrorData](dataLine)
	default:
		return nil, errors.New("unknown event type")
	}
}

func unmarshalEventData[T any](data []byte) (T, error) {
	var eventData T
	err := json.Unmarshal(data, &eventData)
	if err != nil {
		return eventData, err
	}
	return eventData, nil
}

type MessageStartData struct {
	Type    string `json:"type"`
	Message struct {
		ID           string   `json:"id"`
		Type         string   `json:"type"`
		Role         string   `json:"role"`
		Content      []string `json:"content"`
		Model        string   `json:"model"`
		StopReason   string   `json:"stop_reason"`
		StopSequence string   `json:"stop_sequence"`
		Usage        struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type ContentBlockStartData struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content_block"`
}

type PingData struct {
	Type string `json:"type"`
}

type ContentBlockDeltaData struct {
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta TextDelta `json:"delta"`
}

type TextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ContentBlockStopData struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type MessageDeltaData struct {
	Type  string `json:"type"`
	Delta Delta  `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type Delta struct {
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

type MessageStopData struct {
	Type string `json:"type"`
}

type ErrorData struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
