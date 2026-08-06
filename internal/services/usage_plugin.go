package services

import (
	"context"
	"log/slog"

	"vadimgribanov.com/tg-gpt/internal/llm"
)

// UsagePlugin wraps UsersRepo to persist token usage after every model call.
type UsagePlugin struct {
	usersRepo UsersRepo
}

func NewUsagePlugin(usersRepo UsersRepo) *UsagePlugin {
	return &UsagePlugin{usersRepo: usersRepo}
}

func (p *UsagePlugin) Name() string { return "usage" }

var _ AfterModelCallHook = (*UsagePlugin)(nil)

// AfterModelCall persists usage but never fails the turn over it — a metrics-recording
// hiccup shouldn't stop the user from getting their answer, matching prior behavior.
func (p *UsagePlugin) AfterModelCall(ctx context.Context, rc *RunContext, content string, toolCalls []llm.ToolCall, usage llm.Usage) error {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return nil
	}
	// Logged per model call (not just per turn) so cache effectiveness is visible in
	// production: cached_input_tokens should be 0 on the first message of a dialog and
	// climb toward input_tokens on later turns once the stable prefix is being reused.
	slog.InfoContext(ctx, "LLM usage",
		"user_id", rc.User.Id,
		"model", rc.Model,
		"input_tokens", usage.InputTokens,
		"cached_input_tokens", usage.CachedInputTokens,
		"output_tokens", usage.OutputTokens,
	)
	if err := p.usersRepo.AddTokenUsage(rc.User.Id, usage.InputTokens, usage.OutputTokens); err != nil {
		slog.ErrorContext(ctx, "Error updating user token counts", "error", err)
	}
	return nil
}
