package ingestion

import "time"

const MappingVersion = "dataset-v1"

type Options struct {
	SellerZIP      string
	BuyerCSV       string
	SourceTimezone string
}

type Report struct {
	MappingVersion string         `json:"mapping_version"`
	SourceTimezone string         `json:"source_timezone_assumption"`
	Files          []FileReport   `json:"files"`
	Database       *DatabaseStats `json:"database,omitempty"`
}

type DatabaseStats struct {
	Users                   int64 `json:"users"`
	BuyerScopes             int64 `json:"buyer_scopes"`
	SellerScopes            int64 `json:"seller_scopes"`
	Events                  int64 `json:"distinct_events"`
	Projections             int64 `json:"event_projections"`
	AcceptedRevisions       int64 `json:"accepted_revisions"`
	EventsWithoutProjection int64 `json:"events_without_projection"`
	Consistent              bool  `json:"revision_tracking_consistent"`
}

// Reports contain counts and filenames, never customer text or identifiers.
type FileReport struct {
	MappingVersion string         `json:"mapping_version"`
	Name           string         `json:"file"`
	SHA256         string         `json:"sha256"`
	Records        int            `json:"source_records"`
	Accepted       int            `json:"accepted_records"`
	Quarantined    int            `json:"quarantined_records"`
	Issues         map[string]int `json:"issues"`
	Events         int            `json:"normalized_events"`
	NewEvents      int            `json:"new_events"`
	NewProfiles    int            `json:"new_profiles"`
	Skipped        bool           `json:"already_imported"`
}

type Dataset struct{ Files []*File }
type File struct {
	Report   FileReport
	Rows     []Row
	Profiles []Profile
	Events   []Event
}
type Row struct {
	Number  int
	Hash    string
	Payload []byte
	Reason  string
}
type Profile struct {
	GLID       string
	Role       string
	Payload    []byte
	Provenance []byte
}
type Event struct {
	GLID, Role, Source, EntityID, Revision, Kind, Channel, Thread string
	Payload                                                       []byte
	Hash, RawTime, TimeQuality                                    string
	OccurredAt                                                    *time.Time
}
