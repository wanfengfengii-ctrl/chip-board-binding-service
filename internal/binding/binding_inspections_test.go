package binding_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inspectionHitJSON mirrors one entry of the binding-inspection history.
type inspectionHitJSON struct {
	Inspection inspectionJSON `json:"inspection"`
	HitSide    string         `json:"hit_side"`
}

// inspectionHistoryJSON mirrors the wire shape of one history page.
type inspectionHistoryJSON struct {
	Binding    bindingJSON         `json:"binding"`
	Entries    []inspectionHitJSON `json:"entries"`
	NextCursor *int64              `json:"next_cursor"`
}

func parseHistory(t *testing.T, raw []byte) inspectionHistoryJSON {
	t.Helper()
	var h inspectionHistoryJSON
	require.NoError(t, json.Unmarshal(raw, &h))
	return h
}

func bindingInspectionsPath(bindingID int64, query string) string {
	return fmt.Sprintf("/api/v1/bindings/%d/inspections%s", bindingID, query)
}

// TestBindingInspectionsHitSides covers the core history view: every physical
// verification that stored the target binding id — on the chip side, the board
// side or both — comes back once, newest first, with its full record and the
// side it hit on. Inspections that never touched the binding, including
// mismatches between other bindings and unregistered scans, stay out.
func TestBindingInspectionsHitSides(t *testing.T) {
	env := newTestEnv(t)

	status, rawA := env.mustCreate(t, "REQ-HI-A", "CHIP-HI-A", "BOARD-HI-A")
	require.Equal(t, http.StatusCreated, status)
	bindingA := parseBinding(t, rawA)
	status, rawB := env.mustCreate(t, "REQ-HI-B", "CHIP-HI-B", "BOARD-HI-B")
	require.Equal(t, http.StatusCreated, status)
	parseBinding(t, rawB)

	// Five verifications touch binding A, interleaved with two that do not.
	both := env.mustInspect(t, "CHIP-HI-A", "BOARD-HI-A") // CONSISTENT: both
	require.Equal(t, "CONSISTENT", both.Result)
	chipMismatch := env.mustInspect(t, "CHIP-HI-A", "BOARD-HI-B") // MISMATCH: chip side
	require.Equal(t, "MISMATCH", chipMismatch.Result)
	boardMismatch := env.mustInspect(t, "CHIP-HI-B", "BOARD-HI-A") // MISMATCH: board side
	require.Equal(t, "MISMATCH", boardMismatch.Result)
	chipPartial := env.mustInspect(t, "CHIP-HI-A", "BOARD-HI-GHOST") // PARTIAL: chip side
	require.Equal(t, "PARTIAL", chipPartial.Result)
	boardPartial := env.mustInspect(t, "CHIP-HI-GHOST", "BOARD-HI-A") // PARTIAL: board side
	require.Equal(t, "PARTIAL", boardPartial.Result)
	env.mustInspect(t, "CHIP-HI-B", "BOARD-HI-B")          // B consistent: excluded
	env.mustInspect(t, "CHIP-HI-NEVER", "BOARD-HI-NEVER2") // unregistered: excluded

	status, raw := env.get(t, bindingInspectionsPath(bindingA.BindingID, ""))
	require.Equal(t, http.StatusOK, status, string(raw))
	page := parseHistory(t, raw)

	assert.Equal(t, bindingA, page.Binding, "response carries the target binding summary")
	require.Len(t, page.Entries, 5, "exactly the five verifications that touched A")
	assert.Nil(t, page.NextCursor, "everything fits the default page")

	// Newest inspection first; hit sides follow which stored binding id matched.
	want := []struct {
		id   int64
		side string
	}{
		{boardPartial.InspectionID, "board"},
		{chipPartial.InspectionID, "chip"},
		{boardMismatch.InspectionID, "board"},
		{chipMismatch.InspectionID, "chip"},
		{both.InspectionID, "both"},
	}
	for i, w := range want {
		assert.Equal(t, w.id, page.Entries[i].Inspection.InspectionID, "entry %d id", i)
		assert.Equal(t, w.side, page.Entries[i].HitSide, "entry %d (%d) hit side", i, w.id)
		// Strictly descending inspection ids give a fixed order.
		if i > 0 {
			assert.Less(t, page.Entries[i].Inspection.InspectionID, page.Entries[i-1].Inspection.InspectionID)
		}
	}

	// The same record is never returned twice even though both sides matched A.
	seen := map[int64]int{}
	for _, e := range page.Entries {
		seen[e.Inspection.InspectionID]++
	}
	for id, n := range seen {
		assert.Equal(t, 1, n, "inspection %d appears exactly once", id)
	}

	// Every entry carries the full record, byte-structure-equivalent to the
	// inspection review endpoint.
	for _, e := range page.Entries {
		status, got := env.get(t, fmt.Sprintf("%s/%d", inspectionsPath, e.Inspection.InspectionID))
		require.Equal(t, http.StatusOK, status, string(got))
		assert.Equal(t, parseInspection(t, got), e.Inspection, "full record of inspection %d", e.Inspection.InspectionID)
	}

	// The view is read-only: nothing was written anywhere.
	env.assertCounts(t, 2, 2)
	env.assertInspectionCount(t, 7)
}

// mustInspect creates an inspection and requires 201, returning the parsed
// record.
func (e *testEnv) mustInspect(t *testing.T, chip, board string) inspectionJSON {
	t.Helper()
	status, raw := e.inspect(t, chip, board)
	require.Equal(t, http.StatusCreated, status, string(raw))
	return parseInspection(t, raw)
}

// TestBindingInspectionsPagination walks a longer history with small pages:
// consecutive pages share no id, stay in fixed descending order, and the last
// page carries a null cursor.
func TestBindingInspectionsPagination(t *testing.T) {
	env := newTestEnv(t)

	status, rawT := env.mustCreate(t, "REQ-PG-T", "CHIP-PG-T", "BOARD-PG-T")
	require.Equal(t, http.StatusCreated, status)
	target := parseBinding(t, rawT)
	status, rawO := env.mustCreate(t, "REQ-PG-O", "CHIP-PG-O", "BOARD-PG-O")
	require.Equal(t, http.StatusCreated, status)
	parseBinding(t, rawO)

	// Six verifications touch the target; three unrelated ones are interleaved
	// so the history ids must not be assumed contiguous.
	var wantIDs []int64
	env.mustInspect(t, "CHIP-PG-O", "BOARD-PG-O")
	wantIDs = append(wantIDs, env.mustInspect(t, "CHIP-PG-T", "BOARD-PG-T").InspectionID)
	env.mustInspect(t, "CHIP-PG-NOPE", "BOARD-PG-NOPE2")
	wantIDs = append(wantIDs, env.mustInspect(t, "CHIP-PG-T", "BOARD-PG-O").InspectionID)
	wantIDs = append(wantIDs, env.mustInspect(t, "CHIP-PG-O", "BOARD-PG-T").InspectionID)
	env.mustInspect(t, "CHIP-PG-O", "BOARD-PG-GHOST")
	wantIDs = append(wantIDs, env.mustInspect(t, "CHIP-PG-T", "BOARD-PG-GHOST").InspectionID)
	wantIDs = append(wantIDs, env.mustInspect(t, "CHIP-PG-GHOST", "BOARD-PG-T").InspectionID)
	wantIDs = append(wantIDs, env.mustInspect(t, "CHIP-PG-T", "BOARD-PG-T").InspectionID)

	var gotIDs []int64
	var cursor *int64
	pages := 0
	for {
		query := "?limit=2"
		if cursor != nil {
			query = fmt.Sprintf("?limit=2&before_id=%d", *cursor)
		}
		status, raw := env.get(t, bindingInspectionsPath(target.BindingID, query))
		require.Equal(t, http.StatusOK, status, string(raw))
		page := parseHistory(t, raw)
		require.NotEmpty(t, page.Entries, "every page before the end carries entries")
		require.LessOrEqual(t, len(page.Entries), 2)
		gotIDs = append(gotIDs, idsOf(page.Entries)...)
		pages++
		cursor = page.NextCursor
		if cursor == nil {
			break
		}
		require.LessOrEqual(t, pages, 3, "walk terminates")
	}
	require.Equal(t, 3, pages)
	wantDesc := make([]int64, len(wantIDs))
	for i, id := range wantIDs {
		wantDesc[len(wantIDs)-1-i] = id
	}
	assert.Equal(t, wantDesc, gotIDs, "pages reconstruct the full descending history exactly once")

	// Walking the same cursor twice is deterministic and returns the same page.
	status, raw := env.get(t, bindingInspectionsPath(target.BindingID, fmt.Sprintf("?before_id=%d", wantIDs[1])))
	require.Equal(t, http.StatusOK, status, string(raw))
	first := parseHistory(t, raw)
	status, raw = env.get(t, bindingInspectionsPath(target.BindingID, fmt.Sprintf("?before_id=%d", wantIDs[1])))
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Equal(t, first, parseHistory(t, raw))

	// before_id is an exclusive bound: requesting below the newest id omits it.
	status, raw = env.get(t, bindingInspectionsPath(target.BindingID, ""))
	require.Equal(t, http.StatusOK, status, string(raw))
	newestID := parseHistory(t, raw).Entries[0].Inspection.InspectionID
	status, raw = env.get(t, bindingInspectionsPath(target.BindingID, fmt.Sprintf("?before_id=%d", newestID)))
	require.Equal(t, http.StatusOK, status, string(raw))
	for _, e := range parseHistory(t, raw).Entries {
		assert.Less(t, e.Inspection.InspectionID, newestID)
	}
}

// TestBindingInspectionsCursorStableUnderNewWrites pins the keyset guarantee:
// once a client holds a cursor, verifications committed afterwards get higher
// inspection ids and can neither duplicate nor evict rows of the pages already
// keyed below that cursor.
func TestBindingInspectionsCursorStableUnderNewWrites(t *testing.T) {
	env := newTestEnv(t)

	status, rawT := env.mustCreate(t, "REQ-CS-T", "CHIP-CS-T", "BOARD-CS-T")
	require.Equal(t, http.StatusCreated, status)
	target := parseBinding(t, rawT)
	status, rawO := env.mustCreate(t, "REQ-CS-O", "CHIP-CS-O", "BOARD-CS-O")
	require.Equal(t, http.StatusCreated, status)
	parseBinding(t, rawO)

	original := []int64{
		env.mustInspect(t, "CHIP-CS-T", "BOARD-CS-T").InspectionID,
		env.mustInspect(t, "CHIP-CS-T", "BOARD-CS-O").InspectionID,
		env.mustInspect(t, "CHIP-CS-O", "BOARD-CS-T").InspectionID,
		env.mustInspect(t, "CHIP-CS-T", "BOARD-CS-T").InspectionID,
		env.mustInspect(t, "CHIP-CS-T", "BOARD-CS-GHOST").InspectionID,
	}

	// First page with limit=2, then keep its (now stale) cursor.
	status, raw := env.get(t, bindingInspectionsPath(target.BindingID, "?limit=2"))
	require.Equal(t, http.StatusOK, status, string(raw))
	page1 := parseHistory(t, raw)
	require.Equal(t, reverseIDs(original[3:]), idsOf(page1.Entries))
	require.NotNil(t, page1.NextCursor)
	staleCursor := *page1.NextCursor

	// New verifications commit while the client is still paging: one hitting
	// the target (a newer id), one touching another binding, one unregistered.
	newerHit := env.mustInspect(t, "CHIP-CS-O", "BOARD-CS-T").InspectionID
	assert.Greater(t, newerHit, staleCursor)
	env.mustInspect(t, "CHIP-CS-O", "BOARD-CS-O")
	env.mustInspect(t, "CHIP-CS-WAT", "BOARD-CS-WAT2")

	// Continue with the stale cursor: the remaining original pages must come
	// back untouched — the newer hit must not appear, and no original row is
	// skipped or repeated.
	var walked []int64
	cursor := staleCursor
	for cursor != 0 {
		status, raw := env.get(t, bindingInspectionsPath(target.BindingID, fmt.Sprintf("?limit=2&before_id=%d", cursor)))
		require.Equal(t, http.StatusOK, status, string(raw))
		page := parseHistory(t, raw)
		walked = append(walked, idsOf(page.Entries)...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	assert.Equal(t, reverseIDs(original[:3]), walked, "stale cursor pages reconstruct the older history exactly once")
	for _, id := range walked {
		assert.NotEqual(t, newerHit, id, "an inspection committed after paging started never enters an old page")
	}

	// A fresh walk from the head does include the newer hit, exactly once.
	var fresh []int64
	var next *int64
	for {
		query := "?limit=50"
		if next != nil {
			query = fmt.Sprintf("?limit=50&before_id=%d", *next)
		}
		status, raw := env.get(t, bindingInspectionsPath(target.BindingID, query))
		require.Equal(t, http.StatusOK, status, string(raw))
		page := parseHistory(t, raw)
		fresh = append(fresh, idsOf(page.Entries)...)
		if page.NextCursor == nil {
			break
		}
		next = page.NextCursor
	}
	assert.Equal(t, append([]int64{newerHit}, reverseIDs(original)...), fresh)
}

func reverseIDs(ids []int64) []int64 {
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
	}
	return out
}

func idsOf(entries []inspectionHitJSON) []int64 {
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.Inspection.InspectionID
	}
	return ids
}

// TestBindingInspectionsEmptyBeforeCursor covers the end of the walk: a
// cursor before every record yields an empty array and a null cursor without
// writing anything to any ledger.
func TestBindingInspectionsEmptyBeforeCursor(t *testing.T) {
	env := newTestEnv(t)

	status, raw := env.mustCreate(t, "REQ-EMP", "CHIP-EMP", "BOARD-EMP")
	require.Equal(t, http.StatusCreated, status)
	target := parseBinding(t, raw)
	env.mustInspect(t, "CHIP-EMP", "BOARD-EMP")

	// No inspection id is lower than 1.
	status, got := env.get(t, bindingInspectionsPath(target.BindingID, "?before_id=1"))
	require.Equal(t, http.StatusOK, status, string(got))
	assert.Contains(t, string(got), `"entries":[]`, "empty result is a JSON array, not null")
	page := parseHistory(t, got)
	assert.Empty(t, page.Entries)
	assert.NotNil(t, page.Entries)
	assert.Nil(t, page.NextCursor)
	assert.Equal(t, target, page.Binding)

	// A brand-new binding with no history at all behaves the same way.
	status, rawOther := env.mustCreate(t, "REQ-EMP-2", "CHIP-EMP-2", "BOARD-EMP-2")
	require.Equal(t, http.StatusCreated, status)
	other := parseBinding(t, rawOther)
	status, got = env.get(t, bindingInspectionsPath(other.BindingID, ""))
	require.Equal(t, http.StatusOK, status, string(got))
	assert.Contains(t, string(got), `"entries":[]`)
	assert.Nil(t, parseHistory(t, got).NextCursor)

	// The reads wrote no ledger rows of any kind.
	env.assertCounts(t, 2, 2)
	env.assertInspectionCount(t, 1)
}

// TestBindingInspectionsValidationAndNotFound pins the 422/404 contract: an
// illegal binding id, before_id or limit is rejected with the offending field
// before the database is touched, and a legal binding id without a binding is
// a 404.
func TestBindingInspectionsValidationAndNotFound(t *testing.T) {
	env := newTestEnv(t)

	status, rawB := env.mustCreate(t, "REQ-HV", "CHIP-HV", "BOARD-HV")
	require.Equal(t, http.StatusCreated, status)
	bound := parseBinding(t, rawB)

	for _, bad := range []string{"0", "-1", "abc", "1.5", "1e3", "+1", "01", "%201"} {
		t.Run("invalid binding id/"+bad, func(t *testing.T) {
			status, raw := env.get(t, "/api/v1/bindings/"+bad+"/inspections")
			require.Equal(t, http.StatusUnprocessableEntity, status, "id %q: %s", bad, raw)
			errBody := parseError(t, raw)
			assert.Equal(t, "VALIDATION_FAILED", errBody.Error.Code)
			assert.Contains(t, errBody.Error.Details, "binding_id")
		})
	}

	for _, bad := range []string{"0", "-1", "abc", "1.5", "1e3", "+1", "01", "%201"} {
		t.Run("invalid before_id/"+bad, func(t *testing.T) {
			status, raw := env.get(t, bindingInspectionsPath(bound.BindingID, "?before_id="+bad))
			require.Equal(t, http.StatusUnprocessableEntity, status, "before_id %q: %s", bad, raw)
			errBody := parseError(t, raw)
			assert.Equal(t, "VALIDATION_FAILED", errBody.Error.Code)
			assert.Contains(t, errBody.Error.Details, "before_id")
		})
	}

	for _, bad := range []string{"0", "51", "-1", "abc", "1.5", "+5", "05", "100"} {
		t.Run("invalid limit/"+bad, func(t *testing.T) {
			status, raw := env.get(t, bindingInspectionsPath(bound.BindingID, "?limit="+bad))
			require.Equal(t, http.StatusUnprocessableEntity, status, "limit %q: %s", bad, raw)
			errBody := parseError(t, raw)
			assert.Equal(t, "VALIDATION_FAILED", errBody.Error.Code)
			assert.Contains(t, errBody.Error.Details, "limit")
		})
	}

	// An empty limit or before_id value is rejected too.
	status, raw := env.get(t, bindingInspectionsPath(bound.BindingID, "?limit="))
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
	assert.Contains(t, parseError(t, raw).Error.Details, "limit")
	status, raw = env.get(t, bindingInspectionsPath(bound.BindingID, "?before_id="))
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
	assert.Contains(t, parseError(t, raw).Error.Details, "before_id")

	// Parameter validation wins over existence: a malformed id on a missing
	// binding is a 422, proving the database was never consulted.
	status, raw = env.get(t, "/api/v1/bindings/0/inspections")
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
	status, raw = env.get(t, bindingInspectionsPath(999999999, "?before_id=nope"))
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))

	// A legal id without a binding is the ordinary 404.
	status, raw = env.get(t, bindingInspectionsPath(999999999, ""))
	require.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "NOT_FOUND", parseError(t, raw).Error.Code)

	// Validation failures and the 404 never wrote anything.
	env.assertCounts(t, 1, 1)
	env.assertInspectionCount(t, 0)
}

// TestBindingInspectionsDatabaseFailureReturns500 pins the error contract.
func TestBindingInspectionsDatabaseFailureReturns500(t *testing.T) {
	_ = testDSN(t) // skip when no integration database is configured
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	require.NoError(t, err)
	deadPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	deadPool.Close()

	srv := newHTTPServer(deadPool)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/bindings/1/inspections")
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, string(raw))
	assert.Equal(t, "INTERNAL", parseError(t, raw).Error.Code)
}

// TestBindingInspectionsKeepsExistingEndpointsCompatible guards that binding
// create/replay, the single and batch lookups, inspection create/review and
// the mismatch-peers view behave unchanged alongside the new route.
func TestBindingInspectionsKeepsExistingEndpointsCompatible(t *testing.T) {
	env := newTestEnv(t)

	status, created := env.mustCreate(t, "REQ-HC", "CHIP-HC", "BOARD-HC")
	require.Equal(t, http.StatusCreated, status)
	status, replay := env.mustCreate(t, "REQ-HC", "CHIP-HC", "BOARD-HC")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, parseBinding(t, created), parseBinding(t, replay))

	for _, path := range []string{
		"/api/v1/bindings/by-request-key/REQ-HC",
		"/api/v1/bindings/by-chip-uid/CHIP-HC",
		"/api/v1/bindings/by-board-serial/BOARD-HC",
	} {
		status, raw := env.get(t, path)
		require.Equal(t, http.StatusOK, status, path)
		assert.Equal(t, parseBinding(t, created), parseBinding(t, raw), path)
	}

	// Inspection create/review keep their wire shape.
	insp := env.mustInspect(t, "CHIP-HC", "BOARD-HC")
	assert.Equal(t, "CONSISTENT", insp.Result)
	status, got := env.get(t, fmt.Sprintf("%s/%d", inspectionsPath, insp.InspectionID))
	require.Equal(t, http.StatusOK, status, string(got))
	assert.Equal(t, insp, parseInspection(t, got))

	// Batch lookup is unchanged.
	status, raw := env.post(t, "/api/v1/bindings/batch-lookup",
		`{"queries":[{"line":1,"type":"chip_uid","value":"CHIP-HC"}]}`)
	require.Equal(t, http.StatusOK, status, string(raw))
	var batch struct {
		Results []struct {
			Line    int          `json:"line"`
			Status  string       `json:"status"`
			Binding *bindingJSON `json:"binding"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(raw, &batch))
	require.Len(t, batch.Results, 1)
	assert.Equal(t, "FOUND", batch.Results[0].Status)
	require.NotNil(t, batch.Results[0].Binding)
	assert.Equal(t, parseBinding(t, created), *batch.Results[0].Binding)

	// Mismatch peers still aggregate only mismatches.
	status, rawB := env.mustCreate(t, "REQ-HC-2", "CHIP-HC-2", "BOARD-HC-2")
	require.Equal(t, http.StatusCreated, status)
	env.mustInspect(t, "CHIP-HC", "BOARD-HC-2")
	status, raw = env.get(t, fmt.Sprintf("/api/v1/bindings/%d/mismatch-peers", parseBinding(t, created).BindingID))
	require.Equal(t, http.StatusOK, status, string(raw))
	mp := parseMismatchPeers(t, raw)
	require.Len(t, mp.Peers, 1)
	assert.Equal(t, parseBinding(t, rawB), mp.Peers[0].Binding)

	// The new history view sees the consistent and the mismatch verification,
	// once each, and wrote nothing itself.
	status, raw = env.get(t, bindingInspectionsPath(parseBinding(t, created).BindingID, ""))
	require.Equal(t, http.StatusOK, status, string(raw))
	history := parseHistory(t, raw)
	require.Len(t, history.Entries, 2)
	env.assertCounts(t, 2, 2)
	env.assertInspectionCount(t, 2)
}

// TestBindingInspectionsRouteDoesNotShadowLookups guards gin routing: the
// parameterized inspections route must not swallow the static by-* routes.
func TestBindingInspectionsRouteDoesNotShadowLookups(t *testing.T) {
	env := newTestEnv(t)

	status, raw := env.mustCreate(t, "REQ-HRS", "CHIP-HRS", "BOARD-HRS")
	require.Equal(t, http.StatusCreated, status)
	created := parseBinding(t, raw)

	status, raw = env.get(t, "/api/v1/bindings/by-chip-uid/CHIP-HRS")
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Equal(t, created, parseBinding(t, raw))

	status, raw = env.get(t, "/api/v1/bindings/by-chip-uid/inspections")
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
	assert.Equal(t, "VALIDATION_FAILED", parseError(t, raw).Error.Code)

	status, _ = env.get(t, bindingInspectionsPath(created.BindingID, "")+"/extra")
	assert.Equal(t, http.StatusNotFound, status)
}
