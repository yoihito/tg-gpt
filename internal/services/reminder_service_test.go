package services

import (
	"strings"
	"testing"

	"vadimgribanov.com/tg-gpt/internal/models"
)

func TestNormalizeReminderActionDefaultsToNotify(t *testing.T) {
	actionType, actionPrompt, err := normalizeReminderAction("", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if actionType != models.ReminderActionNotify {
		t.Fatalf("action type: got %q", actionType)
	}
	if actionPrompt != "" {
		t.Fatalf("notify action should discard action prompt, got %q", actionPrompt)
	}
}

func TestNormalizeReminderActionRequiresPrompt(t *testing.T) {
	_, _, err := normalizeReminderAction("prompt", " ")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "action_prompt is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeReminderActionAcceptsPrompt(t *testing.T) {
	actionType, actionPrompt, err := normalizeReminderAction("prompt", " search the web ")
	if err != nil {
		t.Fatal(err)
	}
	if actionType != models.ReminderActionPrompt {
		t.Fatalf("action type: got %q", actionType)
	}
	if actionPrompt != "search the web" {
		t.Fatalf("action prompt: got %q", actionPrompt)
	}
}
