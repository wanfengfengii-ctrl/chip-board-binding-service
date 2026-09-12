package binding_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inspectionJSON mirrors the wire shape of one inspection record.
type inspectionJSON struct {
	InspectionID   int64        `json:"inspection_id"`
	ChipUID        string       `json:"chip_uid"`
	BoardSerial    string       `json:"board_serial"`
	Result         string       `json:"result"`
	ChipBindingID  *int64       `json:"chip_binding_id"`
	BoardBindingID *int64       `json:"board_binding_id"`
	CreatedAt      string       `json:"created_at"`
	ChipBinding    *bindingJSON `json:"chip_binding"`
	BoardBinding   *bindingJSON `json:"board_binding"`
}

const inspectionsPath = "/api/v1/inspections"

func parseInspection(t *testing.T, raw []byte) inspectionJSON {
	t.Helper()
	var in inspectionJSON
	require.NoError(t, json.Unmarshal(raw, &in))
	return in
}

func (e *testEnv) inspectRaw(body string) (int, []byte, error) {
	resp, err := http.Post(e.server.URL+inspectionsPath, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := readAll(resp)
	return resp.StatusCode, raw, err
}

func (e *testEnv) inspect(t *testing.T, chip, board string) (int, []byte) {
	t.Helper()
	status, raw, err := e.inspectRaw(fmt.Sprintf(`{"chip_uid":%q,"board_serial":%q}`, chip, board))
	require.NoError(t, err)
	return status, raw
}

// TestInspectionConsistent covers acceptance path 1: a chip and board that
// were registered in the same binding are judged CONSISTENT, the verdict is
// persisted with the scanned values and the matched binding summary, and the
// technician can re-open the original verdict by inspection id.
func TestInspectionConsistent(t *testing.T) {
	env := newTestEnv(t)

	status, raw := env.mustCreate(t, "REQ-IC", "CHIP-IC", "BOARD-IC")
	require.Equal(t, http.StatusCreated, status)
	bound := parseBinding(t, raw)

	status, raw = env.inspect(t, "CHIP-IC", "BOARD-IC")
	require.Equal(t, http.StatusCreated, status, string(raw))
	insp := parseInspection(t, raw)
	assert.Equal(t, "CONSISTENT", insp.Result)
	assert.Positive(t, insp.InspectionID)
	assert.Equal(t, "CHIP-IC", insp.ChipUID)
	assert.Equal(t, "BOARD-IC", insp.BoardSerial)
	assert.NotEmpty(t, insp.CreatedAt)
	require.NotNil(t, insp.ChipBindingID)
	require.NotNil(t, insp.BoardBindingID)
	assert.Equal(t, bound.BindingID, *insp.ChipBindingID)
	assert.Equal(t, bound.BindingID, *insp.BoardBindingID)
	// Both sides carry the summary of the one binding they share.
	require.NotNil(t, insp.ChipBinding)
	require.NotNil(t, insp.BoardBinding)
	assert.Equal(t, bound, *insp.ChipBinding)
	assert.Equal(t, bound, *insp.BoardBinding)

	// The record is persisted and re-readable for review, byte-equivalent.
	status, got := env.get(t, fmt.Sprintf("%s/%d", inspectionsPath, insp.InspectionID))
	require.Equal(t, http.StatusOK, status, string(got))
	assert.Equal(t, insp, parseInspection(t, got))

	env.assertInspectionCount(t, 1)
}

// TestInspectionMismatch covers acceptance path 2: each scanned identifier is
// registered, but to a different binding — the verdict is MISMATCH and each
// side keeps the id and summary of the binding it actually hit.
func TestInspectionMismatch(t *testing.T) {
	env := newTestEnv(t)

	status, rawA := env.mustCreate(t, "REQ-IM-A", "CHIP-IM-A", "BOARD-IM-A")
	require.Equal(t, http.StatusCreated, status)
	bindingA := parseBinding(t, rawA)
	status, rawB := env.mustCreate(t, "REQ-IM-B", "CHIP-IM-B", "BOARD-IM-B")
	require.Equal(t, http.StatusCreated, status)
	bindingB := parseBinding(t, rawB)

	// A chip from binding A scanned with a board from binding B.
	status, raw := env.inspect(t, "CHIP-IM-A", "BOARD-IM-B")
	require.Equal(t, http.StatusCreated, status, string(raw))
	insp := parseInspection(t, raw)
	assert.Equal(t, "MISMATCH", insp.Result)
	require.NotNil(t, insp.ChipBindingID)
	require.NotNil(t, insp.BoardBindingID)
	assert.Equal(t, bindingA.BindingID, *insp.ChipBindingID)
	assert.Equal(t, bindingB.BindingID, *insp.BoardBindingID)
	require.NotNil(t, insp.ChipBinding)
	require.NotNil(t, insp.BoardBinding)
	assert.Equal(t, bindingA, *insp.ChipBinding)
	assert.Equal(t, bindingB, *insp.BoardBinding)

	status, got := env.get(t, fmt.Sprintf("%s/%d", inspectionsPath, insp.InspectionID))
	require.Equal(t, http.StatusOK, status, string(got))
	assert.Equal(t, insp, parseInspection(t, got))

	env.assertInspectionCount(t, 1)
}

// TestInspectionPartial covers acceptance path 3a: exactly one side is
// registered. The hit side carries its binding id and summary; the other side
// stays null.
func TestInspectionPartial(t *testing.T) {
	env := newTestEnv(t)

	status, raw := env.mustCreate(t, "REQ-IP", "CHIP-IP", "BOARD-IP")
	require.Equal(t, http.StatusCreated, status)
	bound := parseBinding(t, raw)

	// Chip registered, board unknown.
	status, rawInsp := env.inspect(t, "CHIP-IP", "BOARD-IP-MISSING")
	require.Equal(t, http.StatusCreated, status, string(rawInsp))
	chipSide := parseInspection(t, rawInsp)
	assert.Equal(t, "PARTIAL", chipSide.Result)
	require.NotNil(t, chipSide.ChipBindingID)
	assert.Equal(t, bound.BindingID, *chipSide.ChipBindingID)
	require.NotNil(t, chipSide.ChipBinding)
	assert.Equal(t, bound, *chipSide.ChipBinding)
	assert.Nil(t, chipSide.BoardBindingID)
	assert.Nil(t, chipSide.BoardBinding)

	// Board registered, chip unknown.
	status, rawInsp = env.inspect(t, "CHIP-IP-MISSING", "BOARD-IP")
	require.Equal(t, http.StatusCreated, status, string(rawInsp))
	boardSide := parseInspection(t, rawInsp)
	assert.Equal(t, "PARTIAL", boardSide.Result)
	assert.Nil(t, boardSide.ChipBindingID)
	assert.Nil(t, boardSide.ChipBinding)
	require.NotNil(t, boardSide.BoardBindingID)
	assert.Equal(t, bound.BindingID, *boardSide.BoardBindingID)
	require.NotNil(t, boardSide.BoardBinding)
	assert.Equal(t, bound, *boardSide.BoardBinding)

	// Both verdicts are independently persistent.
	for _, want := range []inspectionJSON{chipSide, boardSide} {
		status, got := env.get(t, fmt.Sprintf("%s/%d", inspectionsPath, want.InspectionID))
		require.Equal(t, http.StatusOK, status, string(got))
		assert.Equal(t, want, parseInspection(t, got))
	}
	env.assertInspectionCount(t, 2)
}

// TestInspectionUnregistered covers acceptance path 3b: neither scanned
// identifier is registered — the verdict is UNREGISTERED with no binding ids
// or summaries, and it is still stored as an auditable inspection.
func TestInspectionUnregistered(t *testing.T) {
	env := newTestEnv(t)

	status, raw := env.inspect(t, "CHIP-NEVER", "BOARD-NEVER")
	require.Equal(t, http.StatusCreated, status, string(raw))
	insp := parseInspection(t, raw)
	assert.Equal(t, "UNREGISTERED", insp.Result)
	assert.Nil(t, insp.ChipBindingID)
	assert.Nil(t, insp.BoardBindingID)
	assert.Nil(t, insp.ChipBinding)
	assert.Nil(t, insp.BoardBinding)
	assert.Positive(t, insp.InspectionID)

	status, got := env.get(t, fmt.Sprintf("%s/%d", inspectionsPath, insp.InspectionID))
	require.Equal(t, http.StatusOK, status, string(got))
	assert.Equal(t, insp, parseInspection(t, got))

	env.assertInspectionCount(t, 1)
}

// TestInspectionValidationRejected pins the 422 contract for every invalid
// create shape; rejected verifications must leave no inspection rows.
func TestInspectionValidationRejected(t *testing.T) {
	env := newTestEnv(t)

	cases := map[string]string{
		"lowercase chip uid":    `{"chip_uid":"chip-1","board_serial":"BOARD-1"}`,
		"underscore board":      `{"chip_uid":"CHIP-1","board_serial":"BOARD_1"}`,
		"non-ascii chip uid":    `{"chip_uid":"CHIP-é","board_serial":"BOARD-1"}`,
		"empty chip uid":        `{"chip_uid":"","board_serial":"BOARD-1"}`,
		"missing board serial":  `{"chip_uid":"CHIP-1"}`,
		"missing chip uid":      `{"board_serial":"BOARD-1"}`,
		"empty object":          `{}`,
		"overlong board serial": `{"chip_uid":"CHIP-1","board_serial":"` + strings.Repeat("B", 65) + `"}`,
		"malformed json":        `{"chip_uid":`,
		"wrong field type":      `{"chip_uid":42,"board_serial":"BOARD-1"}`,
		"null chip uid":         `{"chip_uid":null,"board_serial":"BOARD-1"}`,
		"unknown field":         `{"chip_uid":"CHIP-1","board_serial":"BOARD-1","extra":1}`,
		"empty body":            ``,
		"trailing json garbage": `{"chip_uid":"CHIP-1","board_serial":"BOARD-1"}{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			status, raw, err := env.inspectRaw(body)
			require.NoError(t, err)
			require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
			assert.Equal(t, "VALIDATION_FAILED", parseError(t, raw).Error.Code)
		})
	}

	// Nothing invalid ever reaches the inspections table, and no bindings are
	// needed to reject it.
	env.assertInspectionCount(t, 0)
}

// TestInspectionGetIDValidationAndNotFound pins the path-id contract: an id
// that is not a positive integer is a 422 validation error (nothing is
// queried), while a legal id without a record is a 404.
func TestInspectionGetIDValidationAndNotFound(t *testing.T) {
	env := newTestEnv(t)

	for _, bad := range []string{"0", "-1", "abc", "1.5", "1e3", "%201", "%2B1", "01"} {
		t.Run("invalid/"+bad, func(t *testing.T) {
			status, raw := env.get(t, inspectionsPath+"/"+bad)
			require.Equal(t, http.StatusUnprocessableEntity, status, "id %q: %s", bad, raw)
			assert.Equal(t, "VALIDATION_FAILED", parseError(t, raw).Error.Code)
		})
	}

	// Legal ids that simply do not exist remain ordinary 404s.
	status, raw := env.get(t, inspectionsPath+"/999999")
	require.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "NOT_FOUND", parseError(t, raw).Error.Code)

	// Validation failures never create records.
	env.assertInspectionCount(t, 0)
}

// TestInspectionImmutableAtDatabaseLevel proves the write-once guarantee where
// it matters: UPDATE and DELETE against a recorded inspection are rejected by
// the database itself.
func TestInspectionImmutableAtDatabaseLevel(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	status, raw := env.inspect(t, "CHIP-IMM", "BOARD-IMM")
	require.Equal(t, http.StatusCreated, status, string(raw))
	id := parseInspection(t, raw).InspectionID

	_, err := env.pool.Exec(ctx, `UPDATE inspections SET result = 'MISMATCH' WHERE id = $1`, id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable")

	_, err = env.pool.Exec(ctx, `DELETE FROM inspections WHERE id = $1`, id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable")

	// The original verdict is still there, unchanged.
	status, got := env.get(t, fmt.Sprintf("%s/%d", inspectionsPath, id))
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "UNREGISTERED", parseInspection(t, got).Result)
	env.assertInspectionCount(t, 1)
}

// TestInspectionSnapshotUnderConcurrentBinds proves "resolved in one
// transaction" under write churn: every matched chip/board pair is committed
// together by another station, so an inspection of the pair must see both
// sides (CONSISTENT) or neither (UNREGISTERED), never a torn PARTIAL. Cross
// pairs must never be judged CONSISTENT.
func TestInspectionSnapshotUnderConcurrentBinds(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	const pairs = 24

	var writers sync.WaitGroup
	writers.Add(pairs)
	for i := 0; i < pairs; i++ {
		go func(i int) {
			defer writers.Done()
			time.Sleep(time.Duration(i%5) * 4 * time.Millisecond)
			tx, err := env.pool.Begin(ctx)
			if err != nil {
				return
			}
			defer tx.Rollback(ctx)
			key := fmt.Sprintf("REQ-ISNAP-%02d", i)
			if _, err := tx.Exec(ctx,
				`INSERT INTO requests (request_key, chip_uid, board_serial) VALUES ($1,$2,$3)`,
				key, fmt.Sprintf("CHIP-ISNAP-%02d", i), fmt.Sprintf("BOARD-ISNAP-%02d", i)); err != nil {
				return
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO bindings (request_key, chip_uid, board_serial) VALUES ($1,$2,$3)`,
				key, fmt.Sprintf("CHIP-ISNAP-%02d", i), fmt.Sprintf("BOARD-ISNAP-%02d", i)); err != nil {
				return
			}
			_ = tx.Commit(ctx)
		}(i)
	}

	var mu sync.Mutex
	var failures []string
	var sawConsistent, sawUnregistered int
	var readers sync.WaitGroup
	for round := 0; round < 12; round++ {
		readers.Add(1)
		go func(round int) {
			defer readers.Done()
			time.Sleep(time.Duration(round) * 3 * time.Millisecond)
			for i := 0; i < pairs; i++ {
				chip := fmt.Sprintf("CHIP-ISNAP-%02d", i)
				matched := fmt.Sprintf("BOARD-ISNAP-%02d", i)

				// The matched pair is committed in one transaction: a single
				// resolution snapshot can never see just one side.
				status, raw, err := env.inspectRaw(fmt.Sprintf(`{"chip_uid":%q,"board_serial":%q}`, chip, matched))
				if err != nil || status != http.StatusCreated {
					continue
				}
				insp := parseInspection(t, raw)
				mu.Lock()
				switch insp.Result {
				case "CONSISTENT":
					sawConsistent++
					if insp.ChipBindingID == nil || insp.BoardBindingID == nil ||
						*insp.ChipBindingID != *insp.BoardBindingID {
						failures = append(failures, fmt.Sprintf("round %d pair %d CONSISTENT with unequal binding ids", round, i))
					}
				case "UNREGISTERED":
					sawUnregistered++
					if insp.ChipBindingID != nil || insp.BoardBindingID != nil {
						failures = append(failures, fmt.Sprintf("round %d pair %d UNREGISTERED with binding ids", round, i))
					}
				default:
					failures = append(failures, fmt.Sprintf("round %d pair %d torn snapshot: %s", round, i, insp.Result))
				}
				mu.Unlock()

				// A cross pair (chip from i, board from a different binding)
				// can never look CONSISTENT at any snapshot.
				j := (i + 1) % pairs
				crossBoard := fmt.Sprintf("BOARD-ISNAP-%02d", j)
				status, raw, err = env.inspectRaw(fmt.Sprintf(`{"chip_uid":%q,"board_serial":%q}`, chip, crossBoard))
				if err != nil || status != http.StatusCreated {
					continue
				}
				cross := parseInspection(t, raw)
				mu.Lock()
				if cross.Result == "CONSISTENT" {
					failures = append(failures, fmt.Sprintf("round %d cross pair %d/%d judged CONSISTENT", round, i, j))
				}
				mu.Unlock()
			}
		}(round)
	}
	readers.Wait()
	writers.Wait()

	for _, f := range failures {
		assert.Fail(t, f)
	}
	assert.Greater(t, sawConsistent, 0, "some matched pairs were observed CONSISTENT mid-churn")

	// After everything settles, every matched pair is CONSISTENT.
	for i := 0; i < pairs; i++ {
		status, raw := env.inspect(t, fmt.Sprintf("CHIP-ISNAP-%02d", i), fmt.Sprintf("BOARD-ISNAP-%02d", i))
		require.Equal(t, http.StatusCreated, status, string(raw))
		assert.Equal(t, "CONSISTENT", parseInspection(t, raw).Result)
	}
}

// TestInspectionDatabaseFailureReturns500 pins the error contract: a database
// failure while resolving or saving is a 500 INTERNAL.
func TestInspectionDatabaseFailureReturns500(t *testing.T) {
	_ = testDSN(t) // skip when no integration database is configured
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	require.NoError(t, err)
	deadPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	deadPool.Close()

	srv := newHTTPServer(deadPool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+inspectionsPath, "application/json",
		strings.NewReader(`{"chip_uid":"CHIP-1","board_serial":"BOARD-1"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, string(raw))
	assert.Equal(t, "INTERNAL", parseError(t, raw).Error.Code)

	// A dead database on the review path is the same envelope.
	resp2, err := http.Get(srv.URL + inspectionsPath + "/1")
	require.NoError(t, err)
	defer resp2.Body.Close()
	raw2, err := readAll(resp2)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp2.StatusCode, string(raw2))
	assert.Equal(t, "INTERNAL", parseError(t, raw2).Error.Code)
}

// TestInspectionKeepsExistingEndpointsCompatible guards that binding create,
// replay and lookups behave unchanged alongside the inspection routes.
func TestInspectionKeepsExistingEndpointsCompatible(t *testing.T) {
	env := newTestEnv(t)

	status, created := env.mustCreate(t, "REQ-XCOMPAT", "CHIP-XCOMPAT", "BOARD-XCOMPAT")
	require.Equal(t, http.StatusCreated, status)
	status, replay := env.mustCreate(t, "REQ-XCOMPAT", "CHIP-XCOMPAT", "BOARD-XCOMPAT")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, parseBinding(t, created), parseBinding(t, replay))

	for _, path := range []string{
		"/api/v1/bindings/by-request-key/REQ-XCOMPAT",
		"/api/v1/bindings/by-chip-uid/CHIP-XCOMPAT",
		"/api/v1/bindings/by-board-serial/BOARD-XCOMPAT",
	} {
		status, raw := env.get(t, path)
		require.Equal(t, http.StatusOK, status, path)
		assert.Equal(t, parseBinding(t, created), parseBinding(t, raw), path)
	}

	// An inspection must not touch the binding ledger.
	status, raw := env.inspect(t, "CHIP-XCOMPAT", "BOARD-XCOMPAT")
	require.Equal(t, http.StatusCreated, status, string(raw))
	assert.Equal(t, "CONSISTENT", parseInspection(t, raw).Result)
	status, raw = env.inspect(t, "CHIP-XCOMPAT", "BOARD-OTHER")
	require.Equal(t, http.StatusCreated, status, string(raw))
	assert.Equal(t, "PARTIAL", parseInspection(t, raw).Result)
	env.assertCounts(t, 1, 1)
}
