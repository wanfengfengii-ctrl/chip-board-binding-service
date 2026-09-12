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

// ErrInspectionNotFound is returned when no inspection matches the requested id.
var ErrInspectionNotFound = errors.New("inspection not found")

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

// batchLookupSQL resolves any mix of the three identifiers in one query. Each
// input row is tagged with its kind ('chip_uid', 'board_serial' or
// 'request_key'); the JOIN finds at most one binding per row because each
// identifier column is unique. Rows without a match yield nothing, exactly
// like the single-item 404 lookups.
const batchLookupSQL = `
SELECT q.line, b.id, r.request_key, r.chip_uid, r.board_serial, r.created_at
FROM unnest($1::bigint[], $2::text[], $3::text[]) AS q(line, kind, value)
JOIN bindings b ON (
	(q.kind = 'chip_uid'     AND b.chip_uid     = q.value) OR
	(q.kind = 'board_serial' AND b.board_serial = q.value) OR
	(q.kind = 'request_key'  AND b.request_key  = q.value)
)
JOIN requests r ON r.request_key = b.request_key`

// BatchLookup resolves many identifiers against one read-only repeatable-read
// transaction snapshot, so a batch under verification never mixes bindings
// from different points in time even while other stations commit new bindings
// concurrently.
//
// Identical (type, value) pairs are queried once and the result is restored
// for every repeated row: the whole batch always costs the same fixed database
// round trips (one BEGIN, one SELECT and one COMMIT) regardless of how many
// items it carries, instead of one round trip per item. Nothing is written to
// the request ledger. Results are returned in input order; misses carry
// StatusNotFound while leaving the other items untouched.
func (s *Store) BatchLookup(ctx context.Context, queries []BatchQuery) ([]BatchItem, error) {
	items := make([]BatchItem, len(queries))
	for i, q := range queries {
		items[i] = BatchItem{Line: q.Line, Type: q.Type, Value: q.Value, Status: StatusNotFound}
	}

	// Deduplicate by (type, value); remember which input rows share each key.
	type queryKey struct {
		Type  LookupType
		Value string
	}
	dedup := make(map[queryKey]struct{})
	keyByLine := make(map[int64]queryKey, len(queries))
	lines := make([]int64, 0, len(queries))
	kinds := make([]string, 0, len(queries))
	values := make([]string, 0, len(queries))
	for _, q := range queries {
		key := queryKey{q.Type, q.Value}
		if _, seen := dedup[key]; seen {
			continue
		}
		dedup[key] = struct{}{}
		// The distinct key is tagged with the line of its first occurrence;
		// the returned line maps straight back to that key.
		keyByLine[int64(q.Line)] = key
		lines = append(lines, int64(q.Line))
		kinds = append(kinds, string(q.Type))
		values = append(values, q.Value)
	}

	// A read-only repeatable-read transaction pins a single snapshot for the
	// whole batch: it cannot take row locks that block concurrent binding
	// creates, and a binding committed while the single batch SELECT is in
	// flight is either visible to every item of the batch or to none.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, batchLookupSQL, lines, kinds, values)
	if err != nil {
		return nil, err
	}
	found := make(map[queryKey]Binding, len(values))
	for rows.Next() {
		var line int64
		var b Binding
		if err := rows.Scan(&line, &b.BindingID, &b.RequestKey, &b.ChipUID, &b.BoardSerial, &b.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		b.CreatedAt = b.CreatedAt.UTC()
		found[keyByLine[line]] = b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// Restore the deduplicated result onto every input row, in input order.
	for i := range items {
		if b, ok := found[queryKey{items[i].Type, items[i].Value}]; ok {
			binding := b
			items[i].Status = StatusFound
			items[i].Binding = &binding
		}
	}
	return items, nil
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

// resolveInspectionSQL resolves both scanned identifiers in a single
// statement. The two LATERAL sub-selects read the bindings/requests tables on
// one snapshot (chip side first, board side second); a side without a
// registration yields null columns. Each sub-select returns at most one row
// because chip_uid and board_serial are unique in bindings.
const resolveInspectionSQL = `
SELECT
    cb.id, cb.request_key, cb.chip_uid, cb.board_serial, cb.created_at,
    bb.id, bb.request_key, bb.chip_uid, bb.board_serial, bb.created_at
FROM (SELECT 1) AS seed
LEFT JOIN LATERAL (
    SELECT b.id, r.request_key, r.chip_uid, r.board_serial, r.created_at
    FROM bindings b
    JOIN requests r ON r.request_key = b.request_key
    WHERE b.chip_uid = $1
) cb ON TRUE
LEFT JOIN LATERAL (
    SELECT b.id, r.request_key, r.chip_uid, r.board_serial, r.created_at
    FROM bindings b
    JOIN requests r ON r.request_key = b.request_key
    WHERE b.board_serial = $2
) bb ON TRUE`

const insertInspectionSQL = `
INSERT INTO inspections (chip_uid, board_serial, result, chip_binding_id, board_binding_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, created_at`

// selectInspectionSQL reads one stored inspection together with a summary of
// the binding each side hit at decision time. Missing sides (null foreign
// keys) yield null summary columns.
const selectInspectionSQL = `
SELECT
    i.id, i.chip_uid, i.board_serial, i.result,
    i.chip_binding_id, i.board_binding_id, i.created_at,
    cb.id, cr.request_key, cr.chip_uid, cr.board_serial, cr.created_at,
    bb.id, br.request_key, br.chip_uid, br.board_serial, br.created_at
FROM inspections i
LEFT JOIN bindings cb ON cb.id = i.chip_binding_id
LEFT JOIN requests cr ON cr.request_key = cb.request_key
LEFT JOIN bindings bb ON bb.id = i.board_binding_id
LEFT JOIN requests br ON br.request_key = bb.request_key
WHERE i.id = $1`

// CreateInspection resolves the scanned chip UID and board serial against the
// existing bindings and persists the verdict in one transaction. The two
// sides are resolved in a single statement (one snapshot), so the verdict can
// never mix bindings from different points in time. The inspection row is
// inserted once and never updated or deleted afterwards.
func (s *Store) CreateInspection(ctx context.Context, req InspectionRequest) (Inspection, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Inspection{}, err
	}
	defer tx.Rollback(ctx)

	var chipID, boardID *int64
	var chipKey, chipUID, chipBoard *string
	var chipCreatedAt *time.Time
	var boardKey, boardChip, boardSerial *string
	var boardCreatedAt *time.Time
	err = tx.QueryRow(ctx, resolveInspectionSQL, req.ChipUID, req.BoardSerial).Scan(
		&chipID, &chipKey, &chipUID, &chipBoard, &chipCreatedAt,
		&boardID, &boardKey, &boardChip, &boardSerial, &boardCreatedAt,
	)
	if err != nil {
		return Inspection{}, err
	}

	result := verdict(chipID, boardID)

	var inspectionID int64
	var createdAt time.Time
	err = tx.QueryRow(ctx, insertInspectionSQL,
		req.ChipUID, req.BoardSerial, string(result), chipID, boardID,
	).Scan(&inspectionID, &createdAt)
	if err != nil {
		return Inspection{}, err
	}

	insp, err := scanInspection(tx.QueryRow(ctx, selectInspectionSQL, inspectionID))
	if err != nil {
		return Inspection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Inspection{}, err
	}
	return insp, nil
}

// verdict derives the inspection outcome from the bindings the two sides hit:
// equal binding ids are CONSISTENT, two different ids are MISMATCH, exactly
// one hit is PARTIAL and neither hit is UNREGISTERED.
func verdict(chipBindingID, boardBindingID *int64) InspectionResult {
	switch {
	case chipBindingID != nil && boardBindingID != nil:
		if *chipBindingID == *boardBindingID {
			return ResultConsistent
		}
		return ResultMismatch
	case chipBindingID != nil || boardBindingID != nil:
		return ResultPartial
	default:
		return ResultUnregistered
	}
}

// GetInspection returns a previously recorded inspection for a repair
// technician reviewing the original verdict.
func (s *Store) GetInspection(ctx context.Context, inspectionID int64) (Inspection, error) {
	return scanInspection(s.pool.QueryRow(ctx, selectInspectionSQL, inspectionID))
}

// scanInspection scans the 17 columns of selectInspectionSQL. The stored
// binding id columns and all summary columns are nullable; a nil summary
// means that side was unregistered.
func scanInspection(row pgx.Row) (Inspection, error) {
	var in Inspection
	var result string
	var chipID, boardID *int64
	var chipKey, chipUID, chipBoard *string
	var chipCreatedAt *time.Time
	var boardKey, boardChip, boardSerial *string
	var boardCreatedAt *time.Time
	err := row.Scan(
		&in.InspectionID, &in.ChipUID, &in.BoardSerial, &result,
		&chipID, &boardID, &in.CreatedAt,
		&chipID, &chipKey, &chipUID, &chipBoard, &chipCreatedAt,
		&boardID, &boardKey, &boardChip, &boardSerial, &boardCreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Inspection{}, ErrInspectionNotFound
	}
	if err != nil {
		return Inspection{}, err
	}
	in.Result = InspectionResult(result)
	in.ChipBindingID = chipID
	in.BoardBindingID = boardID
	in.CreatedAt = in.CreatedAt.UTC()
	if chipID != nil {
		in.ChipBinding = &Binding{
			BindingID:   *chipID,
			RequestKey:  *chipKey,
			ChipUID:     *chipUID,
			BoardSerial: *chipBoard,
			CreatedAt:   chipCreatedAt.UTC(),
		}
	}
	if boardID != nil {
		in.BoardBinding = &Binding{
			BindingID:   *boardID,
			RequestKey:  *boardKey,
			ChipUID:     *boardChip,
			BoardSerial: *boardSerial,
			CreatedAt:   boardCreatedAt.UTC(),
		}
	}
	return in, nil
}

// Ping reports whether the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}
