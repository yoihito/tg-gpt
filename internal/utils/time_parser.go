package utils

import (
	"fmt"
	"strings"
	"time"

	"github.com/olebedev/when"
	"github.com/olebedev/when/rules/common"
	"github.com/olebedev/when/rules/en"
)

type TimeParser struct {
	parser *when.Parser
}

func NewTimeParser() *TimeParser {
	w := when.New(nil)
	w.Add(en.All...)
	w.Add(common.All...)

	return &TimeParser{
		parser: w,
	}
}

// ParseTime extracts time from a natural-language reminder string and returns the
// time plus the remaining message text (after the time expression is removed).
// Used by the one-shot reminder path where the LLM packs both into one string.
func (tp *TimeParser) ParseTime(input string, referenceTime time.Time, timezone *time.Location) (*time.Time, string, error) {
	if timezone == nil {
		timezone = time.UTC
	}
	localRef := referenceTime.In(timezone)

	result, err := tp.parser.Parse(input, localRef)
	if err != nil || result == nil {
		return nil, "", fmt.Errorf("could not parse time from input")
	}

	parsedTime := result.Time
	message := strings.TrimSpace(strings.Replace(input, result.Text, "", 1))
	if message == "" {
		return nil, "", fmt.Errorf("no reminder message provided")
	}
	return &parsedTime, message, nil
}

// ParseTimeOnly parses a natural-language time expression without requiring a residual
// message. Returns just the parsed time.
func (tp *TimeParser) ParseTimeOnly(input string, referenceTime time.Time, timezone *time.Location) (*time.Time, error) {
	if timezone == nil {
		timezone = time.UTC
	}
	localRef := referenceTime.In(timezone)

	result, err := tp.parser.Parse(input, localRef)
	if err != nil || result == nil {
		return nil, fmt.Errorf("could not parse time from %q", input)
	}
	return &result.Time, nil
}

// GetUserTimezone attempts to load timezone; returns an error if the name is invalid.
func GetUserTimezone(timezoneStr string) (*time.Location, error) {
	if timezoneStr == "" {
		return nil, fmt.Errorf("timezone is required")
	}
	loc, err := time.LoadLocation(timezoneStr)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", timezoneStr, err)
	}
	return loc, nil
}
