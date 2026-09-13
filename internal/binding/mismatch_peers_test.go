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

// mismatchPeersJSON mirrors the wire shape of the mismatch-peers response.
type mismatchPeersJSON struct {
	Binding bindingJSON        `json:"binding"`
	Peers   []mismatchPeerJSON `json:"peers"`
}

type mismatchPeerJSON struct {
	Binding          bindingJSON `json:"binding"`
	Occurrences      int64       `json:"occurrences"`
	LastInspectionID int64       `json:"last_inspection_id"`
	LastOccurredAt   string      `json:"last_occurred_at"`
}

func parseMismatchPeers(t *testing.T, raw []byte) mismatchPeersJSON {
	t.Helper()
	var mp mismatchPeersJSON
	require.NoError(t, json.Unmarshal(raw, &mp))
	return mp
}

func mismatchPeersPath(bindingID int64) string {
	return fmt.Sprintf("/api/v1/bindings/%d/mismatch-peers", bindingID)
}

// mustMismatch inspects a chip/board pair and requires a MISMATCH verdict,
// returning the recorded inspection.
func (e *testEnv) mustMismatch(t *testing.T, chip, board string) inspectionJSON {
	t.Helper()
	status, raw := e.inspect(t, chip, board)
	require.Equal(t, http.StatusCreated, status, string(raw))
	insp := parseInspection(t, raw)
	require.Equal(t, "MISMATCH", insp.Result, "setup inspection must be a mismatch")
	return insp
}

// TestMismatchPeersAggregationAndOrder covers the supervisor triage view: one
// binding implicated with several others is returned with each peer's summary,
// the co-occurrence count, the most recent inspection id and its timestamp,
// most repeated first.
func TestMismatchPeersAggregationAndOrder(t *testing.T) {
	env := newTestEnv(t)

	status, rawA := env.mustCreate(t, "REQ-MP-A", "CHIP-MP-A", "BOARD-MP-A")
	require.Equal(t, http.StatusCreated, status)
	bindingA := parseBinding(t, rawA)
	status, rawB := env.mustCreate(t, "REQ-MP-B", "CHIP-MP-B", "BOARD-MP-B")
	require.Equal(t, http.StatusCreated, status)
	bindingB := parseBinding(t, rawB)
	status, rawC := env.mustCreate(t, "REQ-MP-C", "CHIP-MP-C", "BOARD-MP-C")
	require.Equal(t, http.StatusCreated, status)
	bindingC := parseBinding(t, rawC)

	// A and B mismatch twice (once in each scan direction); A and C once.
	env.mustMismatch(t, "CHIP-MP-A", "BOARD-MP-B")
	latestAB := env.mustMismatch(t, "CHIP-MP-B", "BOARD-MP-A")
	latestAC := env.mustMismatch(t, "CHIP-MP-A", "BOARD-MP-C")

	status, raw := env.get(t, mismatchPeersPath(bindingA.BindingID))
	require.Equal(t, http.StatusOK, status, string(raw))
	res := parseMismatchPeers(t, raw)

	assert.Equal(t, bindingA, res.Binding, "response carries the target binding summary")
	require.Len(t, res.Peers, 2)

	// Most repeated relation first: B with two occurrences.
	assert.Equal(t, bindingB, res.Peers[0].Binding)
	assert.Equal(t, int64(2), res.Peers[0].Occurrences)
	assert.Equal(t, latestAB.InspectionID, res.Peers[0].LastInspectionID)
	assert.Equal(t, latestAB.CreatedAt, res.Peers[0].LastOccurredAt)

	// Then C with a single occurrence.
	assert.Equal(t, bindingC, res.Peers[1].Binding)
	assert.Equal(t, int64(1), res.Peers[1].Occurrences)
	assert.Equal(t, latestAC.InspectionID, res.Peers[1].LastInspectionID)
	assert.Equal(t, latestAC.CreatedAt, res.Peers[1].LastOccurredAt)

	// The lookup is read-only: nothing was written anywhere.
	env.assertCounts(t, 3, 3)
	env.assertInspectionCount(t, 3)
}

// TestMismatchPeersBidirectionalConsistency proves both sides of one mix-up
// relation observe the same count and the same most recent inspection when
// each is queried as the target.
func TestMismatchPeersBidirectionalConsistency(t *testing.T) {
	env := newTestEnv(t)

	status, rawA := env.mustCreate(t, "REQ-BD-A", "CHIP-BD-A", "BOARD-BD-A")
	require.Equal(t, http.StatusCreated, status)
	bindingA := parseBinding(t, rawA)
	status, rawB := env.mustCreate(t, "REQ-BD-B", "CHIP-BD-B", "BOARD-BD-B")
	require.Equal(t, http.StatusCreated, status)
	bindingB := parseBinding(t, rawB)

	env.mustMismatch(t, "CHIP-BD-A", "BOARD-BD-B")
	env.mustMismatch(t, "CHIP-BD-B", "BOARD-BD-A")
	latest := env.mustMismatch(t, "CHIP-BD-A", "BOARD-BD-B")

	status, raw := env.get(t, mismatchPeersPath(bindingA.BindingID))
	require.Equal(t, http.StatusOK, status, string(raw))
	fromA := parseMismatchPeers(t, raw)
	require.Len(t, fromA.Peers, 1)
	assert.Equal(t, bindingB, fromA.Peers[0].Binding)

	status, raw = env.get(t, mismatchPeersPath(bindingB.BindingID))
	require.Equal(t, http.StatusOK, status, string(raw))
	fromB := parseMismatchPeers(t, raw)
	require.Len(t, fromB.Peers, 1)
	assert.Equal(t, bindingA, fromB.Peers[0].Binding)

	// The same relation, seen from either side: identical count, identical
	// most recent inspection and timestamp.
	assert.Equal(t, fromA.Peers[0].Occurrences, fromB.Peers[0].Occurrences)
	assert.Equal(t, int64(3), fromA.Peers[0].Occurrences)
	assert.Equal(t, fromA.Peers[0].LastInspectionID, fromB.Peers[0].LastInspectionID)
	assert.Equal(t, latest.InspectionID, fromA.Peers[0].LastInspectionID)
	assert.Equal(t, fromA.Peers[0].LastOccurredAt, fromB.Peers[0].LastOccurredAt)
	assert.Equal(t, latest.CreatedAt, fromA.Peers[0].LastOccurredAt)
}

// TestMismatchPeersLatestInspectionPins proves that with several inspections
// interleaved across relations, each peer's "most recent" fields point at
// that relation's own newest inspection, not some other relation's.
func TestMismatchPeersLatestInspectionPins(t *testing.T) {
	env := newTestEnv(t)

	status, rawA := env.mustCreate(t, "REQ-LP-A", "CHIP-LP-A", "BOARD-LP-A")
	require.Equal(t, http.StatusCreated, status)
	bindingA := parseBinding(t, rawA)
	status, rawB := env.mustCreate(t, "REQ-LP-B", "CHIP-LP-B", "BOARD-LP-B")
	require.Equal(t, http.StatusCreated, status)
	bindingB := parseBinding(t, rawB)
	status, rawC := env.mustCreate(t, "REQ-LP-C", "CHIP-LP-C", "BOARD-LP-C")
	require.Equal(t, http.StatusCreated, status)
	bindingC := parseBinding(t, rawC)

	// Interleave the two relations; each relation's last record sits at a
	// different point in the inspection sequence.
	env.mustMismatch(t, "CHIP-LP-A", "BOARD-LP-B")
	env.mustMismatch(t, "CHIP-LP-A", "BOARD-LP-C")
	latestAB := env.mustMismatch(t, "CHIP-LP-B", "BOARD-LP-A")
	latestAC := env.mustMismatch(t, "CHIP-LP-C", "BOARD-LP-A")

	status, raw := env.get(t, mismatchPeersPath(bindingA.BindingID))
	require.Equal(t, http.StatusOK, status, string(raw))
	res := parseMismatchPeers(t, raw)
	require.Len(t, res.Peers, 2)

	byPeer := map[int64]mismatchPeerJSON{}
	for _, p := range res.Peers {
		byPeer[p.Binding.BindingID] = p
	}
	peerB, ok := byPeer[bindingB.BindingID]
	require.True(t, ok, "binding B is a peer")
	assert.Equal(t, int64(2), peerB.Occurrences)
	assert.Equal(t, latestAB.InspectionID, peerB.LastInspectionID)
	assert.Equal(t, latestAB.CreatedAt, peerB.LastOccurredAt)

	peerC, ok := byPeer[bindingC.BindingID]
	require.True(t, ok, "binding C is a peer")
	assert.Equal(t, int64(2), peerC.Occurrences)
	assert.Equal(t, latestAC.InspectionID, peerC.LastInspectionID)
	assert.Equal(t, latestAC.CreatedAt, peerC.LastOccurredAt)

	// The overall newest inspection (A-C) is not attributed to the A-B
	// relation and vice versa.
	assert.NotEqual(t, peerB.LastInspectionID, peerC.LastInspectionID)
}

// TestMismatchPeersTieOrderingAndLimit pins the deterministic order among
// peers with equal occurrence counts (most recent inspection first) and the
// effect of the limit parameter.
func TestMismatchPeersTieOrderingAndLimit(t *testing.T) {
	env := newTestEnv(t)

	status, rawT := env.mustCreate(t, "REQ-TO-T", "CHIP-TO-T", "BOARD-TO-T")
	require.Equal(t, http.StatusCreated, status)
	target := parseBinding(t, rawT)
	peerIDs := make([]int64, 0, 3)
	for _, name := range []string{"P1", "P2", "P3"} {
		status, raw := env.mustCreate(t, "REQ-TO-"+name, "CHIP-TO-"+name, "BOARD-TO-"+name)
		require.Equal(t, http.StatusCreated, status)
		peerIDs = append(peerIDs, parseBinding(t, raw).BindingID)
		// One mismatch per peer, recorded in peer id order; ties on count.
		env.mustMismatch(t, "CHIP-TO-T", "BOARD-TO-"+name)
	}

	// Equal counts: the peer with the most recent inspection comes first.
	status, raw := env.get(t, mismatchPeersPath(target.BindingID))
	require.Equal(t, http.StatusOK, status, string(raw))
	res := parseMismatchPeers(t, raw)
	require.Len(t, res.Peers, 3)
	assert.Equal(t, peerIDs[2], res.Peers[0].Binding.BindingID)
	assert.Equal(t, peerIDs[1], res.Peers[1].Binding.BindingID)
	assert.Equal(t, peerIDs[0], res.Peers[2].Binding.BindingID)
	for _, p := range res.Peers {
		assert.Equal(t, int64(1), p.Occurrences)
	}
	assert.Greater(t, res.Peers[0].LastInspectionID, res.Peers[1].LastInspectionID)
	assert.Greater(t, res.Peers[1].LastInspectionID, res.Peers[2].LastInspectionID)

	// The limit clips the deterministic ordering, keeping the head.
	status, raw = env.get(t, mismatchPeersPath(target.BindingID)+"?limit=2")
	require.Equal(t, http.StatusOK, status, string(raw))
	limited := parseMismatchPeers(t, raw)
	require.Len(t, limited.Peers, 2)
	assert.Equal(t, res.Peers[:2], limited.Peers)

	status, raw = env.get(t, mismatchPeersPath(target.BindingID)+"?limit=1")
	require.Equal(t, http.StatusOK, status, string(raw))
	limited = parseMismatchPeers(t, raw)
	require.Len(t, limited.Peers, 1)
	assert.Equal(t, res.Peers[0], limited.Peers[0])

	// The maximum limit and the implicit default return everything.
	status, raw = env.get(t, mismatchPeersPath(target.BindingID)+"?limit=50")
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Equal(t, res.Peers, parseMismatchPeers(t, raw).Peers)
}

// TestMismatchPeersEmptyAndNonMismatchExcluded proves only MISMATCH records
// feed the aggregation: consistent, partial and unregistered inspections
// leave the peer list empty, and the empty list is a JSON array, not null.
func TestMismatchPeersEmptyAndNonMismatchExcluded(t *testing.T) {
	env := newTestEnv(t)

	status, rawS := env.mustCreate(t, "REQ-NM-S", "CHIP-NM-S", "BOARD-NM-S")
	require.Equal(t, http.StatusCreated, status)
	bindingS := parseBinding(t, rawS)
	status, rawU := env.mustCreate(t, "REQ-NM-U", "CHIP-NM-U", "BOARD-NM-U")
	require.Equal(t, http.StatusCreated, status)
	bindingU := parseBinding(t, rawU)

	// One of each non-mismatch verdict involving the two bindings.
	status, raw := env.inspect(t, "CHIP-NM-S", "BOARD-NM-S")
	require.Equal(t, http.StatusCreated, status, string(raw))
	require.Equal(t, "CONSISTENT", parseInspection(t, raw).Result)
	status, raw = env.inspect(t, "CHIP-NM-S", "BOARD-NM-GHOST")
	require.Equal(t, http.StatusCreated, status, string(raw))
	require.Equal(t, "PARTIAL", parseInspection(t, raw).Result)
	status, raw = env.inspect(t, "CHIP-NM-GHOST", "BOARD-NM-U")
	require.Equal(t, http.StatusCreated, status, string(raw))
	require.Equal(t, "PARTIAL", parseInspection(t, raw).Result)
	status, raw = env.inspect(t, "CHIP-NM-GHOST", "BOARD-NM-GHOST2")
	require.Equal(t, http.StatusCreated, status, string(raw))
	require.Equal(t, "UNREGISTERED", parseInspection(t, raw).Result)

	for _, id := range []int64{bindingS.BindingID, bindingU.BindingID} {
		status, raw := env.get(t, mismatchPeersPath(id))
		require.Equal(t, http.StatusOK, status, string(raw))
		assert.Contains(t, string(raw), `"peers":[]`, "empty result is a JSON array, not null")
		res := parseMismatchPeers(t, raw)
		assert.Empty(t, res.Peers)
		assert.NotNil(t, res.Peers)
	}

	// A mismatch against one of them then appears — proving the empty result
	// above was about verdict kinds, not about the bindings being invisible.
	env.mustMismatch(t, "CHIP-NM-S", "BOARD-NM-U")
	status, raw = env.get(t, mismatchPeersPath(bindingS.BindingID))
	require.Equal(t, http.StatusOK, status, string(raw))
	res := parseMismatchPeers(t, raw)
	require.Len(t, res.Peers, 1)
	assert.Equal(t, bindingU, res.Peers[0].Binding)
	assert.Equal(t, int64(1), res.Peers[0].Occurrences)
}

// TestMismatchPeersValidationAndNotFound pins the 422/404 contract: an
// illegal binding id or an out-of-range limit is rejected before anything is
// queried, and a legal id without a binding is a 404.
func TestMismatchPeersValidationAndNotFound(t *testing.T) {
	env := newTestEnv(t)

	status, rawB := env.mustCreate(t, "REQ-MPV", "CHIP-MPV", "BOARD-MPV")
	require.Equal(t, http.StatusCreated, status)
	bound := parseBinding(t, rawB)

	for _, bad := range []string{"0", "-1", "abc", "1.5", "1e3", "+1", "01", "%201"} {
		t.Run("invalid id/"+bad, func(t *testing.T) {
			status, raw := env.get(t, "/api/v1/bindings/"+bad+"/mismatch-peers")
			require.Equal(t, http.StatusUnprocessableEntity, status, "id %q: %s", bad, raw)
			errBody := parseError(t, raw)
			assert.Equal(t, "VALIDATION_FAILED", errBody.Error.Code)
			assert.Contains(t, errBody.Error.Details, "binding_id")
		})
	}

	for _, bad := range []string{"0", "51", "-1", "abc", "1.5", "+5", "05", "100"} {
		t.Run("invalid limit/"+bad, func(t *testing.T) {
			status, raw := env.get(t, fmt.Sprintf("%s?limit=%s", mismatchPeersPath(bound.BindingID), bad))
			require.Equal(t, http.StatusUnprocessableEntity, status, "limit %q: %s", bad, raw)
			errBody := parseError(t, raw)
			assert.Equal(t, "VALIDATION_FAILED", errBody.Error.Code)
			assert.Contains(t, errBody.Error.Details, "limit")
		})
	}

	// An empty limit value is rejected too.
	status, raw := env.get(t, mismatchPeersPath(bound.BindingID)+"?limit=")
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
	assert.Contains(t, parseError(t, raw).Error.Details, "limit")

	// A legal id without a binding is the ordinary 404.
	status, raw = env.get(t, mismatchPeersPath(999999999))
	require.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "NOT_FOUND", parseError(t, raw).Error.Code)

	// Validation failures and the 404 never wrote anything.
	env.assertCounts(t, 1, 1)
	env.assertInspectionCount(t, 0)
}

// TestMismatchPeersDatabaseFailureReturns500 pins the error contract: a
// database failure on the lookup is the existing 500 INTERNAL envelope.
func TestMismatchPeersDatabaseFailureReturns500(t *testing.T) {
	_ = testDSN(t) // skip when no integration database is configured
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	require.NoError(t, err)
	deadPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	deadPool.Close()

	srv := newHTTPServer(deadPool)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/bindings/1/mismatch-peers")
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, string(raw))
	assert.Equal(t, "INTERNAL", parseError(t, raw).Error.Code)
}

// TestMismatchPeersKeepsExistingEndpointsCompatible guards that binding
// create/replay, the three lookups and the inspection endpoints behave
// unchanged alongside the mismatch-peers route.
func TestMismatchPeersKeepsExistingEndpointsCompatible(t *testing.T) {
	env := newTestEnv(t)

	status, created := env.mustCreate(t, "REQ-MPC", "CHIP-MPC", "BOARD-MPC")
	require.Equal(t, http.StatusCreated, status)
	status, replay := env.mustCreate(t, "REQ-MPC", "CHIP-MPC", "BOARD-MPC")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, parseBinding(t, created), parseBinding(t, replay))

	for _, path := range []string{
		"/api/v1/bindings/by-request-key/REQ-MPC",
		"/api/v1/bindings/by-chip-uid/CHIP-MPC",
		"/api/v1/bindings/by-board-serial/BOARD-MPC",
	} {
		status, raw := env.get(t, path)
		require.Equal(t, http.StatusOK, status, path)
		assert.Equal(t, parseBinding(t, created), parseBinding(t, raw), path)
	}

	// Inspections still record and review exactly as before.
	status, raw := env.inspect(t, "CHIP-MPC", "BOARD-MPC")
	require.Equal(t, http.StatusCreated, status, string(raw))
	insp := parseInspection(t, raw)
	assert.Equal(t, "CONSISTENT", insp.Result)
	status, got := env.get(t, fmt.Sprintf("%s/%d", inspectionsPath, insp.InspectionID))
	require.Equal(t, http.StatusOK, status, string(got))
	assert.Equal(t, insp, parseInspection(t, got))

	// The peers lookup itself wrote nothing.
	status, raw = env.get(t, mismatchPeersPath(parseBinding(t, created).BindingID))
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), `"peers":[]`)
	env.assertCounts(t, 1, 1)
	env.assertInspectionCount(t, 1)
}

// TestMismatchPeersRouteDoesNotShadowLookups guards the gin routing: the
// parameterized /bindings/:binding_id/mismatch-peers route must not swallow
// the static by-* lookup routes.
func TestMismatchPeersRouteDoesNotShadowLookups(t *testing.T) {
	env := newTestEnv(t)

	status, raw := env.mustCreate(t, "REQ-RS", "CHIP-RS", "BOARD-RS")
	require.Equal(t, http.StatusCreated, status)
	created := parseBinding(t, raw)

	status, raw = env.get(t, "/api/v1/bindings/by-chip-uid/CHIP-RS")
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Equal(t, created, parseBinding(t, raw))

	// A non-numeric segment where the binding id would be is a 422 on the
	// peers route, not a swallowed lookup.
	status, raw = env.get(t, "/api/v1/bindings/by-chip-uid/mismatch-peers")
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
	assert.Equal(t, "VALIDATION_FAILED", parseError(t, raw).Error.Code)

	// Trailing slashes or extra segments do not exist.
	status, _ = env.get(t, "/api/v1/bindings/"+fmt.Sprint(created.BindingID)+"/mismatch-peers/extra")
	assert.Equal(t, http.StatusNotFound, status)
}
