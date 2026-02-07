package models

import "time"

type RecurrenceType string

const (
	RecurrenceTypeDaily   RecurrenceType = "daily"
	RecurrenceTypeWeekly  RecurrenceType = "weekly"
	RecurrenceTypeMonthly RecurrenceType = "monthly"
)

type Reminder struct {
	ID                 int64
	UserID             int64
	Message            string
	RemindAt           time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	IsFired            bool
	IsCancelled        bool
	Timezone           string
	IsRecurring        bool
	RecurrenceType     *RecurrenceType
	RecurrenceInterval int
	RecurrenceEndAt    *time.Time
	LastFiredAt        *time.Time
}

// ShouldFire checks if reminder is due
func (r *Reminder) ShouldFire(now time.Time) bool {
	if r.IsCancelled || r.IsFired {
		return false
	}
	return now.After(r.RemindAt) || now.Equal(r.RemindAt)
}

// CalculateNextOccurrence calculates the next time a recurring reminder should fire
func (r *Reminder) CalculateNextOccurrence() *time.Time {
	if !r.IsRecurring || r.RecurrenceType == nil {
		return nil
	}

	baseTime := r.RemindAt
	if r.LastFiredAt != nil {
		baseTime = *r.LastFiredAt
	}

	var next time.Time
	switch *r.RecurrenceType {
	case RecurrenceTypeDaily:
		next = baseTime.AddDate(0, 0, r.RecurrenceInterval)
	case RecurrenceTypeWeekly:
		next = baseTime.AddDate(0, 0, 7*r.RecurrenceInterval)
	case RecurrenceTypeMonthly:
		next = baseTime.AddDate(0, r.RecurrenceInterval, 0)
	default:
		return nil
	}

	// Check if we've passed the end date
	if r.RecurrenceEndAt != nil && next.After(*r.RecurrenceEndAt) {
		return nil
	}

	return &next
}

// HasExpiredRecurrence checks if recurring reminder has reached its end
func (r *Reminder) HasExpiredRecurrence() bool {
	if !r.IsRecurring {
		return false
	}
	if r.RecurrenceEndAt == nil {
		return false // No end date means infinite recurrence
	}
	return time.Now().After(*r.RecurrenceEndAt)
}
