// Package binding implements the chip/board one-to-one binding service:
// an idempotent create endpoint backed by a request ledger and immutable
// binding records, plus lookups by any of the three identifiers.
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
