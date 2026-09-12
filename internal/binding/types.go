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
