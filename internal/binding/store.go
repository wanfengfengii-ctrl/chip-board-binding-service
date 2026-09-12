package binding

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no binding matches the requested identifier.
var ErrNotFound = errors.New("binding not found")

// Outcome describes how a create attempt resolved.
type Outcome string

const (
	// OutcomeCreated means this transaction wrote the ledger row and binding.
	OutcomeCreated Outcome = "created"
	// OutcomeReplayed means the same request key with an identical payload
	// was already committed; the original record is returned unchanged.
	OutcomeReplayed Outcome = "replayed"
	// OutcomeRequestKeyConflict means the request key exists with a
	// different payload.
	OutcomeRequestKeyConflict Outcome = "request_key_conflict"
	// OutcomeDeviceOccupied means the chip UID or board serial is already
	// bound by a different request key.
	OutcomeDeviceOccupied Outcome = "device_occupied"
)

// CreateResult is the resolution of a create attempt. Binding is populated
// for OutcomeCreated and OutcomeReplayed, and carries the pre-existing record
// for OutcomeRequestKeyConflict. Field names the occupied identifier for
// OutcomeDeviceOccupied.
type CreateResult struct {
	Outcome Outcome
	Binding Binding
	Field   string
}

// Store persists bindings in PostgreSQL. Every concurrency guarantee comes
// from database unique constraints and transactions; no in-process locking
// is used, so multiple service replicas can run safely.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const insertRequestSQL = `
INSERT INTO requests (request_key, chip_uid, board_serial)
VALUES ($1, $2, $3)
ON CONFLICT (request_key) DO NOTHING
RETURNING created_at`

const insertBindingSQL = `
INSERT INTO bindings (request_key, chip_uid, board_serial)
VALUES ($1, $2, $3)
RETURNING id`

// Create writes the request ledger row and the one-to-one binding in a single
// transaction. Competing transactions are serialized by the unique
// constraints: ON CONFLICT waits for any in-flight transaction holding the
// same request key to commit or roll back, and a unique violation on the
// bindings table tells the loser exactly which device was already taken.
// Existing rows are never updated.
func (s *Store) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer tx.Rollback(ctx)

	var createdAt time.Time
	err = tx.QueryRow(ctx, insertRequestSQL, req.RequestKey, req.ChipUID, req.BoardSerial).Scan(&createdAt)
	switch {
	case err == nil:
		// Ledger row inserted by this transaction; fall through to the
		// binding insert below.
	case errors.Is(err, pgx.ErrNoRows):
		// The request key already exists. ON CONFLICT waited for any
		// in-flight transaction to resolve first, so the committed row
		// (and its binding, written in the same transaction) is visible.
		existing, qerr := getByRequestKey(ctx, tx, req.RequestKey)
		if qerr != nil {
			return CreateResult{}, qerr
		}
		if existing.ChipUID != req.ChipUID || existing.BoardSerial != req.BoardSerial {
			return CreateResult{Outcome: OutcomeRequestKeyConflict, Binding: existing}, nil
		}
		return CreateResult{Outcome: OutcomeReplayed, Binding: existing}, nil
	default:
		return CreateResult{}, err
	}

	var bindingID int64
	err = tx.QueryRow(ctx, insertBindingSQL, req.RequestKey, req.ChipUID, req.BoardSerial).Scan(&bindingID)
	if err != nil {
		if field, ok := occupiedField(err); ok {
			// The whole transaction (including the ledger row) rolls back:
			// rejected attempts leave no trace.
			return CreateResult{Outcome: OutcomeDeviceOccupied, Field: field}, nil
		}
		return CreateResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{
		Outcome: OutcomeCreated,
		Binding: Binding{
			BindingID:   bindingID,
			RequestKey:  req.RequestKey,
			ChipUID:     req.ChipUID,
			BoardSerial: req.BoardSerial,
			CreatedAt:   createdAt.UTC(),
		},
	}, nil
}

// occupiedField maps a unique violation on the bindings table to the
// identifier that is already bound.
func occupiedField(err error) (field string, ok bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return "", false
	}
	switch pgErr.ConstraintName {
	case "bindings_chip_uid_unique":
		return "chip_uid", true
	case "bindings_board_serial_unique":
		return "board_serial", true
	}
	return "", false
}

const selectBindingSQL = `
SELECT b.id, r.request_key, r.chip_uid, r.board_serial, r.created_at
FROM requests r
JOIN bindings b ON b.request_key = r.request_key
`

// queryer is satisfied by both *pgxpool.Pool and pgx.Tx.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getByRequestKey(ctx context.Context, q queryer, requestKey string) (Binding, error) {
	return scanBinding(q.QueryRow(ctx, selectBindingSQL+`WHERE r.request_key = $1`, requestKey))
}

// GetByRequestKey looks up a binding by the client-generated request key.
func (s *Store) GetByRequestKey(ctx context.Context, requestKey string) (Binding, error) {
	return getByRequestKey(ctx, s.pool, requestKey)
}

// GetByChipUID looks up a binding by chip UID.
func (s *Store) GetByChipUID(ctx context.Context, chipUID string) (Binding, error) {
	return scanBinding(s.pool.QueryRow(ctx, selectBindingSQL+`WHERE r.chip_uid = $1`, chipUID))
}

// GetByBoardSerial looks up a binding by board serial number.
func (s *Store) GetByBoardSerial(ctx context.Context, boardSerial string) (Binding, error) {
	return scanBinding(s.pool.QueryRow(ctx, selectBindingSQL+`WHERE r.board_serial = $1`, boardSerial))
}

func scanBinding(row pgx.Row) (Binding, error) {
	var b Binding
	err := row.Scan(&b.BindingID, &b.RequestKey, &b.ChipUID, &b.BoardSerial, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	if err != nil {
		return Binding{}, err
	}
	b.CreatedAt = b.CreatedAt.UTC()
	return b, nil
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}
