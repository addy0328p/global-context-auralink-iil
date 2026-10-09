// Package memory compiles local, source-backed baseline memory. It does not
// claim that free-text conversations have undergone model-based fact extraction.
package memory

import (
	"encoding/json"
	"time"
)

const RendererVersion = "structured-baseline-v4"

type Scope struct {
	Tenant    string `json:"tenant_id"`
	Namespace string `json:"namespace"`
	GLID      string `json:"glid"`
	Role      string `json:"role"`
}

type Policy struct {
	AsOfDate         string `json:"as_of_source_date"`
	ActivityDays     int    `json:"activity_lookback_days"`
	ConversationDays int    `json:"conversation_lookback_days"`
	MaxBytes         int    `json:"max_markdown_bytes"`
	ThreadRef        string `json:"requested_thread_ref,omitempty"`
}

type Event struct {
	ID                                                               int64
	Revision                                                         int64
	Source, EntityID, Kind, Channel, ThreadRef, RawTime, TimeQuality string
	OccurredAt                                                       *time.Time
	Payload                                                          map[string]any
}

type Input struct {
	Scope               Scope
	Profile, Provenance map[string]any
	Events              []Event
	Synthetic           bool
	Revision            int64
}

type Evidence struct {
	EventID     string      `json:"event_id,omitempty"`
	Source      string      `json:"source"`
	EntityID    string      `json:"source_entity_id,omitempty"`
	Field       string      `json:"field,omitempty"`
	RawTime     string      `json:"source_time_raw,omitempty"`
	TimeQuality string      `json:"time_quality"`
	OccurredAt  *time.Time  `json:"occurred_at,omitempty"`
	SourceHash  string      `json:"source_file_sha256,omitempty"`
	SourceRow   json.Number `json:"source_record,omitempty"`
}

type Fact struct {
	Field     string     `json:"field"`
	Value     string     `json:"value"`
	Status    string     `json:"status"`
	ThreadRef string     `json:"thread_ref,omitempty"`
	Evidence  []Evidence `json:"evidence"`
}

type Work struct {
	ThreadRef string   `json:"thread_ref"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Known     []Fact   `json:"known_fields"`
	Evidence  Evidence `json:"evidence"`
}

type Coverage struct {
	Source             string `json:"source"`
	Channel            string `json:"channel"`
	Total              int    `json:"total_events"`
	InWindow           int    `json:"in_activity_window"`
	OutsideWindow      int    `json:"outside_activity_window"`
	Undated            int    `json:"date_unresolved"`
	Future             int    `json:"after_reference_date"`
	TimezoneUnresolved int    `json:"timezone_unresolved"`
}

type Excerpt struct {
	Channel  string   `json:"channel"`
	Label    string   `json:"label"`
	Text     string   `json:"text"`
	Evidence Evidence `json:"evidence"`
}

type Context struct {
	// These names are shared with Aditya's agent configuration.
	MemoryMD             string     `json:"memory_md"`
	MemoryVersion        int64      `json:"memory_version"`
	ActiveRole           string     `json:"active_role"`
	ActiveThreadRef      string     `json:"active_thread_ref"`
	MissingFields        []string   `json:"missing_fields"`
	NextQuestion         string     `json:"next_question"`
	CurrentConstraints   []Fact     `json:"current_constraints"`
	PreferredLanguage    string     `json:"preferred_language"`
	RendererVersion      string     `json:"renderer_version"`
	Scope                Scope      `json:"scope"`
	Policy               Policy     `json:"selection_policy"`
	IncludedRevision     int64      `json:"included_revision"`
	Historical           bool       `json:"historical"`
	Synthetic            bool       `json:"synthetic"`
	ColdStart            bool       `json:"cold_start"`
	ExtractionMode       string     `json:"extraction_mode"`
	MissingFieldsMeaning string     `json:"missing_fields_meaning"`
	ProfileFacts         []Fact     `json:"profile_facts"`
	Requirements         []Work     `json:"saved_requirements"`
	SourceCoverage       []Coverage `json:"source_coverage"`
	Excerpts             []Excerpt  `json:"conversation_excerpts"`
	OmittedItems         int        `json:"items_omitted_from_markdown"`
	UntimedExcerpts      int        `json:"undated_excerpts_included"`
	Bytes                int        `json:"markdown_bytes"`
	TokenEstimate        int        `json:"token_estimate"`
	TokenMeasurement     string     `json:"token_measurement"`
	PolicyHash           string     `json:"policy_sha256"`
}

type Snapshot struct {
	Scope            Scope     `json:"scope"`
	Version          int64     `json:"memory_version"`
	IncludedRevision int64     `json:"included_revision"`
	MD               string    `json:"memory_md"`
	Hash             string    `json:"content_sha256"`
	Context          Context   `json:"context"`
	PublishedAt      time.Time `json:"published_at"`
	Reused           bool      `json:"reused"`
}

type Candidate struct {
	Scope        Scope    `json:"scope"`
	Channels     []string `json:"channels"`
	Events       int64    `json:"event_count"`
	TextEvents   int64    `json:"conversation_records"`
	Requirements int64    `json:"requirement_threads"`
}
