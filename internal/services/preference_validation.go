package services

import (
	"fmt"
	"strings"
	"time"
)

// preferenceKeyTimezone is the canonical key under which the user's IANA timezone
// lives. ReminderService reads it at fire time, so a malformed value silently breaks
// recurring reminders — strict validation protects both write paths.
const preferenceKeyTimezone = "timezone"

// ValidatePreference enforces format rules for preference values where the format
// matters. Returns (humanReadableHint, error) on rejection; both empty on success.
//
// The hint is what callers should show to the LLM (or the user) — it explains how
// to fix the value. The error is the structured reason.
func ValidatePreference(key, value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case preferenceKeyTimezone:
		return validateTimezoneValue(value)
	}
	return "", nil
}

func validateTimezoneValue(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "timezone must be a non-empty IANA name (e.g. 'Europe/Berlin').",
			fmt.Errorf("timezone is empty")
	}
	if _, err := time.LoadLocation(trimmed); err != nil {
		return fmt.Sprintf(
			"timezone value %q is not a valid IANA name. Use a bare IANA identifier like 'Europe/Berlin' or 'America/New_York' — no descriptions, country names, or extra text.",
			trimmed,
		), fmt.Errorf("invalid IANA timezone %q: %w", trimmed, err)
	}
	return "", nil
}
