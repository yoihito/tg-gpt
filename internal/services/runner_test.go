package services

import (
	"context"
	"fmt"
	"testing"

	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/models"
)

func TestAsToolDelegatesToSubAgent(t *testing.T) {
	subClient := &fakeLLMClient{
		models: map[string]struct{}{"sub-model": {}},
		streams: [][]llm.StreamEvent{
			{{TextDelta: "42 is the answer."}},
		},
	}
	subDef := Definition{
		Name:  "researcher",
		Model: "sub-model",
		BuildSystemPrompt: func(ctx context.Context) (string, error) {
			return "You are a research sub-agent.", nil
		},
		Tools: NewToolSet(),
	}
	// No plugins: the sub-agent's own delegation exchange shouldn't need memory/usage
	// side effects to demonstrate the mechanism.
	subRunner := NewRunner(subClient)

	toolDef, handler := AsTool(subDef, subRunner, SubAgentToolOptions{
		Description: "Delegates a research task to a sub-agent.",
	})

	parentTools := NewToolSet()
	parentTools.Register(toolDef, handler)

	parentClient := &fakeLLMClient{
		models: map[string]struct{}{"parent-model": {}},
		streams: [][]llm.StreamEvent{
			{{ToolCalls: []llm.ToolCall{{
				ID:        "call_1",
				Index:     0,
				Name:      toolDef.Name,
				Arguments: `{"task":"What is the answer to everything?"}`,
			}}}},
			{{TextDelta: "The sub-agent says: 42 is the answer."}},
		},
	}
	parentDef := Definition{
		Name:  "parent",
		Model: "parent-model",
		BuildSystemPrompt: func(ctx context.Context) (string, error) {
			return "You are the parent agent.", nil
		},
		Tools: parentTools,
	}
	parentRunner := NewRunner(parentClient)

	stream, err := parentRunner.Run(context.Background(), parentDef, models.User{Id: 1}, TurnContext{UserID: 1, DialogID: 1}, []UserInput{
		{Message: llm.Message{Role: llm.RoleUser, Content: "Ask the sub-agent something."}},
	}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next() {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("parent run error: %v", err)
	}

	result := stream.Result()
	if result.Response != "The sub-agent says: 42 is the answer." {
		t.Fatalf("parent response: got %q", result.Response)
	}

	subRequests := subClient.requestsSnapshot()
	if len(subRequests) != 1 {
		t.Fatalf("sub-agent requests: got %d want 1", len(subRequests))
	}
	lastMsg := subRequests[0].Messages[len(subRequests[0].Messages)-1]
	if lastMsg.Role != llm.RoleUser || lastMsg.Content != "What is the answer to everything?" {
		t.Fatalf("sub-agent input message: got %#v", lastMsg)
	}
}

func TestRunnerEmitsCapMessageAsStreamDelta(t *testing.T) {
	streams := make([][]llm.StreamEvent, 0, maxToolIterations)
	for i := 0; i < maxToolIterations; i++ {
		streams = append(streams, []llm.StreamEvent{
			{ToolCalls: []llm.ToolCall{{
				ID:        fmt.Sprintf("call_%d", i),
				Index:     0,
				Name:      "noop",
				Arguments: "{}",
			}}},
		})
	}
	client := &fakeLLMClient{
		models:  map[string]struct{}{"model": {}},
		streams: streams,
	}
	tools := NewToolSet()
	tools.Register(llm.Tool{Name: "noop", Description: "does nothing"}, func(ctx context.Context, mctx TurnContext, user models.User, call llm.ToolCall) (string, error) {
		return "ok", nil
	})
	def := Definition{
		Model: "model",
		BuildSystemPrompt: func(ctx context.Context) (string, error) {
			return "system", nil
		},
		Tools: tools,
	}
	runner := NewRunner(client)

	stream, err := runner.Run(context.Background(), def, models.User{Id: 1}, TurnContext{UserID: 1, DialogID: 1}, []UserInput{
		{Message: llm.Message{Role: llm.RoleUser, Content: "loop forever"}},
	}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var deltas []string
	for stream.Next() {
		ev := stream.Event()
		if ev.Kind == RunEventStreamDelta && ev.StreamEvent.TextDelta != "" {
			deltas = append(deltas, ev.StreamEvent.TextDelta)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(deltas) == 0 {
		t.Fatal("expected the iteration-cap message to be emitted as a real stream delta, not just persisted to trace")
	}
	if last := deltas[len(deltas)-1]; last != capMessage {
		t.Fatalf("last delta: got %q want cap message", last)
	}
	if stream.Result().Response != capMessage {
		t.Fatalf("result response: got %q", stream.Result().Response)
	}
}
