package models

const (
	PreferenceSourceExplicit = "explicit"
	PreferenceSourceInferred = "inferred"
)

type Preference struct {
	ID            int64
	UserID        int64
	PrefKey       string
	PrefValue     string
	Source        string
	SourceTraceID *int64
	CreatedAt     int64
	UpdatedAt     int64
}
