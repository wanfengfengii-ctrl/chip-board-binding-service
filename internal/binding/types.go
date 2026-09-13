// Package binding implements the chip/board one-to-one binding service:
// an idempotent create endpoint backed by a request ledger and immutable
// binding records, plus single and batch lookups by any of the three
// identifiers.
package binding

import "time"

// CreateRequest is the payload of POST /api/v1/bindings. The client (a burn-in
// station) generates RequestKey to make the request idempotent; ChipUID and
// BoardSerial identify the physical parts being paired.
type CreateRequest struct {
	RequestKey  string `json:"request_key"`
	ChipUID     string `json:"chip_uid"`
	BoardSerial string `json:"board_serial"`
}

// Binding is the immutable pairing of one chip UID to one board serial
// number, recorded together with the request key that created it.
type Binding struct {
	BindingID   int64     `json:"binding_id"`
	RequestKey  string    `json:"request_key"`
	ChipUID     string    `json:"chip_uid"`
	BoardSerial string    `json:"board_serial"`
	CreatedAt   time.Time `json:"created_at"`
}

// LookupType selects which of the three identifiers a batch query item
// carries. Exactly one identifier is given per item.
type LookupType string

const (
	// LookupByChipUID queries a binding by its chip UID.
	LookupByChipUID LookupType = "chip_uid"
	// LookupByBoardSerial queries a binding by its board serial number.
	LookupByBoardSerial LookupType = "board_serial"
	// LookupByRequestKey queries a binding by its client-generated request key.
	LookupByRequestKey LookupType = "request_key"
)

// LookupStatus is the per-item outcome of a batch lookup.
type LookupStatus string

const (
	// StatusFound means the identifier resolved to a binding.
	StatusFound LookupStatus = "FOUND"
	// StatusNotFound means no binding carries the identifier; this is a
	// normal per-item result, not an error.
	StatusNotFound LookupStatus = "NOT_FOUND"
)

// MaxBatchQueries bounds how many numbered items one batch lookup may carry:
// a repair technician scans between one and one hundred boards at a time.
const MaxBatchQueries = 100

// BatchQueryItem is one item of a batch lookup request as decoded from the
// wire. Line is a pointer so a missing or null line is distinguishable from
// line 0 during validation.
type BatchQueryItem struct {
	Line  *int   `json:"line"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// BatchQueryRequest is the payload of POST /api/v1/bindings/batch-lookup.
// Each numbered item resolves by exactly one of the three identifiers.
type BatchQueryRequest struct {
	Queries []BatchQueryItem `json:"queries"`
}

// BatchQuery is one validated, normalized lookup item.
type BatchQuery struct {
	Line  int
	Type  LookupType
	Value string
}

// BatchItem is the resolution of one numbered lookup. The query coordinates
// (Line, Type, Value) are echoed back so the station matches results to the
// scanned rows; Binding is populated only when Status is StatusFound.
type BatchItem struct {
	Line    int          `json:"line"`
	Type    LookupType   `json:"type"`
	Value   string       `json:"value"`
	Status  LookupStatus `json:"status"`
	Binding *Binding     `json:"binding,omitempty"`
}

// Mismatch-peer lookup bounds: a repair supervisor reviews between one and
// fifty implicated bindings at a time. DefaultMismatchPeerLimit applies when
// the query carries no limit.
const (
	DefaultMismatchPeerLimit = 50
	MaxMismatchPeerLimit     = 50
)

// MismatchPeer is one binding implicated with the target binding by mismatch
// inspections: how often the pair was recorded together, the most recent
// inspection id and when that most recent mismatch happened.
type MismatchPeer struct {
	Binding          Binding   `json:"binding"`
	Occurrences      int64     `json:"occurrences"`
	LastInspectionID int64     `json:"last_inspection_id"`
	LastOccurredAt   time.Time `json:"last_occurred_at"`
}

// MismatchPeers is the response of the mismatch-peers lookup: the target
// binding summary plus every binding it was mismatched against, ordered for
// triage (most repeated first). Peers is empty — never null — when the target
// has no mismatch history.
type MismatchPeers struct {
	Binding Binding        `json:"binding"`
	Peers   []MismatchPeer `json:"peers"`
}

// InspectionRequest is the payload of POST /api/v1/inspections. Before
// teardown a repair technician scans a chip and the board it is soldered to in
// one physical verification; ChipUID and BoardSerial carry the two scanned
// identifiers.
type InspectionRequest struct {
	ChipUID     string `json:"chip_uid"`
	BoardSerial string `json:"board_serial"`
}

// InspectionResult is the verdict of a physical verification, derived solely
// from the bindings the two scanned identifiers hit.
type InspectionResult string

const (
	// ResultConsistent means both identifiers hit the same binding.
	ResultConsistent InspectionResult = "CONSISTENT"
	// ResultMismatch means both identifiers hit bindings, but different ones.
	ResultMismatch InspectionResult = "MISMATCH"
	// ResultPartial means exactly one of the two identifiers hit a binding.
	ResultPartial InspectionResult = "PARTIAL"
	// ResultUnregistered means neither identifier is registered.
	ResultUnregistered InspectionResult = "UNREGISTERED"
)

// Inspection is the immutable record of one physical verification. It stores
// the scanned values and the verdict together with the nullable IDs of the
// bindings each side resolved to (nil when that side was unregistered), so a
// technician reviewing an old inspection sees exactly what was decided then.
type Inspection struct {
	InspectionID   int64            `json:"inspection_id"`
	ChipUID        string           `json:"chip_uid"`
	BoardSerial    string           `json:"board_serial"`
	Result         InspectionResult `json:"result"`
	ChipBindingID  *int64           `json:"chip_binding_id"`
	BoardBindingID *int64           `json:"board_binding_id"`
	CreatedAt      time.Time        `json:"created_at"`
	ChipBinding    *Binding         `json:"chip_binding,omitempty"`
	BoardBinding   *Binding         `json:"board_binding,omitempty"`
}

// Binding-inspection-history bounds: a repair supervisor pages through at
// most fifty physical verifications per request. DefaultInspectionHistoryLimit
// applies when the query carries no limit.
const (
	DefaultInspectionHistoryLimit = 50
	MaxInspectionHistoryLimit     = 50
)

// HitSide says on which side of a physical verification the queried binding
// was recorded.
type HitSide string

const (
	// HitSideChip means the binding was stored as the chip-side binding.
	HitSideChip HitSide = "chip"
	// HitSideBoard means the binding was stored as the board-side binding.
	HitSideBoard HitSide = "board"
	// HitSideBoth means the same record stored the binding on both sides;
	// a CONSISTENT verification hits the binding exactly once, not twice.
	HitSideBoth HitSide = "both"
)

// InspectionHit is one physical verification in a binding's history: the full
// immutable inspection record together with the side (or both sides) on which
// the queried binding was hit.
type InspectionHit struct {
	Inspection Inspection `json:"inspection"`
	HitSide    HitSide    `json:"hit_side"`
}

// InspectionHistory is one page of a binding's physical-verification history.
// Entries are ordered by inspection id descending and each entry appears at
// most once even when both sides hit the same binding. NextCursor is the id to
// pass as before_id for the following page; it is null on the last page.
// Entries is empty — never null — when no record precedes the cursor.
type InspectionHistory struct {
	Binding    Binding         `json:"binding"`
	Entries    []InspectionHit `json:"entries"`
	NextCursor *int64          `json:"next_cursor"`
}

// ValidateInspectionRequest checks the two scanned identifiers with the same
// 1-64 character identifier rule as every other endpoint.
func ValidateInspectionRequest(req InspectionRequest) *ValidationError {
	fields := make(map[string]string)
	if !identPattern.MatchString(req.ChipUID) {
		fields["chip_uid"] = identRule
	}
	if !identPattern.MatchString(req.BoardSerial) {
		fields["board_serial"] = identRule
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}
