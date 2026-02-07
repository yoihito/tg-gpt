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

// ParseTime extracts time from natural language input
// Returns parsed time and the remaining message text after removing the time expression
func (tp *TimeParser) ParseTime(input string, referenceTime time.Time, timezone *time.Location) (*time.Time, string, error) {
	if timezone == nil {
		timezone = time.UTC
	}

	// Adjust reference time to user's timezone
	localRef := referenceTime.In(timezone)

	result, err := tp.parser.Parse(input, localRef)
	if err != nil || result == nil {
		return nil, "", fmt.Errorf("could not parse time from input")
	}

	// Extract the parsed time
	parsedTime := result.Time

	// Remove the time expression from the input to get the reminder message
	message := strings.TrimSpace(strings.Replace(input, result.Text, "", 1))
	if message == "" {
		return nil, "", fmt.Errorf("no reminder message provided")
	}

	return &parsedTime, message, nil
}

// ParseRecurrence extracts recurrence pattern from text
// Returns recurrence type, interval, and cleaned message
func (tp *TimeParser) ParseRecurrence(input string) (*string, int, string) {
	input = strings.ToLower(input)

	patterns := map[string]struct {
		recurrence string
		interval   int
	}{
		"daily":          {"daily", 1},
		"every day":      {"daily", 1},
		"weekly":         {"weekly", 1},
		"every week":     {"weekly", 1},
		"monthly":        {"monthly", 1},
		"every month":    {"monthly", 1},
		"every 2 days":   {"daily", 2},
		"every 3 days":   {"daily", 3},
		"every 2 weeks":  {"weekly", 2},
		"every 2 months": {"monthly", 2},
	}

	for pattern, config := range patterns {
		if strings.Contains(input, pattern) {
			cleanedMessage := strings.TrimSpace(strings.Replace(input, pattern, "", 1))
			return &config.recurrence, config.interval, cleanedMessage
		}
	}

	return nil, 1, input
}

// GetUserTimezone attempts to load timezone or defaults to UTC
func GetUserTimezone(timezoneStr string) (*time.Location, error) {
	if timezoneStr == "" {
		return time.UTC, nil
	}

	loc, err := time.LoadLocation(timezoneStr)
	if err != nil {
		return time.UTC, fmt.Errorf("invalid timezone: %w", err)
	}

	return loc, nil
}
