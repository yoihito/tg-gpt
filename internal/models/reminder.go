package models

import "time"

type RecurrenceType string
type ReminderActionType string

const (
	RecurrenceTypeDaily   RecurrenceType = "daily"
	RecurrenceTypeWeekly  RecurrenceType = "weekly"
	RecurrenceTypeMonthly RecurrenceType = "monthly"

	ReminderActionNotify ReminderActionType = "notify"
	ReminderActionPrompt ReminderActionType = "prompt"
)

type Reminder struct {
	ID                  int64
	UserID              int64
	Message             string
	RemindAt            time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	IsFired             bool
	IsCancelled         bool
	IsProcessing        bool
	IsRecurring         bool
	RecurrenceType      *RecurrenceType
	RecurrenceInterval  int
	RecurrenceEndAt     *time.Time
	LastFiredAt         *time.Time
	ProcessingStartedAt *time.Time
	ActionType          ReminderActionType
	ActionPrompt        string
}

func (r *Reminder) ShouldFire(now time.Time) bool {
	if r.IsCancelled || r.IsFired {
		return false
	}
	return now.After(r.RemindAt) || now.Equal(r.RemindAt)
}

// CalculateNextOccurrence returns the next scheduled time after r.RemindAt for a
// recurring reminder. Calendar arithmetic is done in the user's timezone so daily/
// weekly/monthly intervals respect DST and month boundaries.
//
// Base is always r.RemindAt (the previous scheduled time), NOT r.LastFiredAt, so
// the schedule doesn't drift due to scheduler polling lag or downtime.
func (r *Reminder) CalculateNextOccurrence(loc *time.Location) *time.Time {
	if !r.IsRecurring || r.RecurrenceType == nil {
		return nil
	}
	if loc == nil {
		loc = time.UTC
	}
	interval := r.RecurrenceInterval
	if interval < 1 {
		interval = 1
	}

	base := r.RemindAt.In(loc)
	var next time.Time
	switch *r.RecurrenceType {
	case RecurrenceTypeDaily:
		next = base.AddDate(0, 0, interval)
	case RecurrenceTypeWeekly:
		next = base.AddDate(0, 0, 7*interval)
	case RecurrenceTypeMonthly:
		next = addMonthsClampDay(base, interval)
	default:
		return nil
	}

	if r.RecurrenceEndAt != nil && next.After(*r.RecurrenceEndAt) {
		return nil
	}
	return &next
}

// HasExpiredRecurrence reports whether the recurrence end has already passed.
func (r *Reminder) HasExpiredRecurrence() bool {
	if !r.IsRecurring || r.RecurrenceEndAt == nil {
		return false
	}
	return time.Now().After(*r.RecurrenceEndAt)
}

// addMonthsClampDay adds N months in the given location, clamping the day-of-month
// to the last day of the target month so "monthly on the 31st" doesn't roll over
// into the next month.
func addMonthsClampDay(base time.Time, months int) time.Time {
	loc := base.Location()
	year := base.Year()
	month := int(base.Month()) + months
	for month > 12 {
		month -= 12
		year++
	}
	for month < 1 {
		month += 12
		year--
	}
	// Day count of target month: day 0 of (month+1) == last day of month.
	daysInTarget := time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, loc).Day()
	day := base.Day()
	if day > daysInTarget {
		day = daysInTarget
	}
	return time.Date(year, time.Month(month), day,
		base.Hour(), base.Minute(), base.Second(), base.Nanosecond(), loc)
}
