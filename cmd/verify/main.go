// Command verify is a one-shot acceptance client for the binding API. It
// exercises the production-line failure modes — a replayed request after a
// lost response, and two stations racing for the same identifiers — and
// exits non-zero if any check fails. Identifiers are suffixed with a unique
// run id so the service can be re-run against the same database.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	codeValidationFailed   = "VALIDATION_FAILED"
	codeRequestKeyConflict = "REQUEST_KEY_CONFLICT"
	codeDeviceAlreadyBound = "DEVICE_ALREADY_BOUND"
	codeNotFound           = "NOT_FOUND"
	codeInternal           = "INTERNAL"
)

type binding struct {
	BindingID   int64  `json:"binding_id"`
	RequestKey  string `json:"request_key"`
	ChipUID     string `json:"chip_uid"`
	BoardSerial string `json:"board_serial"`
	CreatedAt   string `json:"created_at"`
}

// inspection mirrors the wire shape of one physical verification record.
type inspection struct {
	InspectionID   int64    `json:"inspection_id"`
	ChipUID        string   `json:"chip_uid"`
	BoardSerial    string   `json:"board_serial"`
	Result         string   `json:"result"`
	ChipBindingID  *int64   `json:"chip_binding_id"`
	BoardBindingID *int64   `json:"board_binding_id"`
	CreatedAt      string   `json:"created_at"`
	ChipBinding    *binding `json:"chip_binding"`
	BoardBinding   *binding `json:"board_binding"`
}

type errorEnvelope struct {
	Error struct {
		Code        string       `json:"code"`
		Field       string       `json:"field"`
		FieldErrors []fieldError `json:"field_errors"`
	} `json:"error"`
}

type fieldError struct {
	Location string `json:"location"`
	Message  string `json:"message"`
}

// batchQuery is one numbered item of a batch lookup request.
type batchQuery struct {
	Line  int    `json:"line"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type batchRequest struct {
	Queries []batchQuery `json:"queries"`
}

type batchItem struct {
	Line    int      `json:"line"`
	Type    string   `json:"type"`
	Value   string   `json:"value"`
	Status  string   `json:"status"`
	Binding *binding `json:"binding"`
}

type batchResponseBody struct {
	Results []batchItem `json:"results"`
}

type batchResponse struct {
	status  int
	results []batchItem
	errBody errorEnvelope
	raw     []byte
}

type response struct {
	status  int
	binding binding
	errBody errorEnvelope
	raw     []byte
}

type inspectionResponse struct {
	status     int
	inspection inspection
	errBody    errorEnvelope
	raw        []byte
}

// mismatchPeer mirrors one implicated binding of the mismatch-peers response.
type mismatchPeer struct {
	Binding          binding `json:"binding"`
	Occurrences      int64   `json:"occurrences"`
	LastInspectionID int64   `json:"last_inspection_id"`
	LastOccurredAt   string  `json:"last_occurred_at"`
}

// mismatchPeersBody mirrors the wire shape of the mismatch-peers response.
type mismatchPeersBody struct {
	Binding binding        `json:"binding"`
	Peers   []mismatchPeer `json:"peers"`
}

type mismatchPeersResponse struct {
	status  int
	body    mismatchPeersBody
	errBody errorEnvelope
	raw     []byte
}

type verifier struct {
	base     string
	client   *http.Client
	run      string
	failures int
}

func main() {
	base := os.Getenv("API_BASE_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	v := &verifier{
		base:   base,
		client: &http.Client{Timeout: 10 * time.Second},
		run:    fmt.Sprintf("V%d", time.Now().UnixNano()),
	}

	if err := v.waitHealthy(90 * time.Second); err != nil {
		fmt.Printf("FAIL: api not healthy: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("verify run %s against %s\n", v.run, v.base)

	v.scenarioReplayAfterLostResponse()
	v.scenarioRequestKeyConflict()
	v.scenarioDeviceOccupied()
	v.scenarioConcurrentStations()
	v.scenarioValidationRejected()
	v.scenarioBatchMixedWithMissing()
	v.scenarioBatchDuplicatesReturnedPerRow()
	v.scenarioBatchSnapshotUnderConcurrentWrites()
	v.scenarioBatchValidationRejected()
	v.scenarioInspectionVerdicts()
	v.scenarioInspectionReviewAndErrors()
	v.scenarioMismatchPeers()

	fmt.Println()
	if v.failures > 0 {
		fmt.Printf("VERIFY FAILED: %d check(s) failed\n", v.failures)
		os.Exit(1)
	}
	fmt.Println("VERIFY PASSED")
}

// scenarioReplayAfterLostResponse simulates a station that committed a
// binding but never saw the response, then re-sent the identical request.
func (v *verifier) scenarioReplayAfterLostResponse() {
	fmt.Println("scenario: replay after lost response")
	key := "REQ-" + v.run + "-REPLAY"
	chip := "CHIP-" + v.run + "-REPLAY"
	board := "BOARD-" + v.run + "-REPLAY"

	first := v.create(key, chip, board)
	v.check(first.status == http.StatusCreated, "first create returns 201 (got %d: %s)", first.status, first.raw)

	replay := v.create(key, chip, board)
	v.check(replay.status == http.StatusOK, "identical replay returns 200 (got %d: %s)", replay.status, replay.raw)
	v.check(replay.binding == first.binding, "replay returns the original record unchanged")

	byKey := v.get("/api/v1/bindings/by-request-key/" + key)
	v.check(byKey.status == http.StatusOK && byKey.binding == first.binding, "query by request key returns the original record")
	byChip := v.get("/api/v1/bindings/by-chip-uid/" + chip)
	v.check(byChip.status == http.StatusOK && byChip.binding == first.binding, "query by chip UID returns the original record")
	byBoard := v.get("/api/v1/bindings/by-board-serial/" + board)
	v.check(byBoard.status == http.StatusOK && byBoard.binding == first.binding, "query by board serial returns the original record")
}

// scenarioRequestKeyConflict reuses a committed request key with a different
// payload and expects a distinguishable conflict error.
func (v *verifier) scenarioRequestKeyConflict() {
	fmt.Println("scenario: same request key with different payload")
	key := "REQ-" + v.run + "-KEYCONF"
	chip := "CHIP-" + v.run + "-KEYCONF"
	board := "BOARD-" + v.run + "-KEYCONF"

	first := v.create(key, chip, board)
	v.check(first.status == http.StatusCreated, "setup create returns 201 (got %d: %s)", first.status, first.raw)

	conflict := v.create(key, "CHIP-"+v.run+"-KEYCONF-ALT", "BOARD-"+v.run+"-KEYCONF-ALT")
	v.check(conflict.status == http.StatusConflict, "same key with different payload returns 409 (got %d: %s)", conflict.status, conflict.raw)
	v.check(conflict.errBody.Error.Code == codeRequestKeyConflict, "error code is REQUEST_KEY_CONFLICT (got %q)", conflict.errBody.Error.Code)

	still := v.get("/api/v1/bindings/by-request-key/" + key)
	v.check(still.status == http.StatusOK && still.binding == first.binding, "original record is never rewritten")
}

// scenarioDeviceOccupied binds a chip and a board, then confirms that any
// other request key referencing either device is rejected, and that the
// rejected attempts leave no trace.
func (v *verifier) scenarioDeviceOccupied() {
	fmt.Println("scenario: device already occupied")
	key := "REQ-" + v.run + "-OCC"
	chip := "CHIP-" + v.run + "-OCC"
	board := "BOARD-" + v.run + "-OCC"

	first := v.create(key, chip, board)
	v.check(first.status == http.StatusCreated, "setup create returns 201 (got %d: %s)", first.status, first.raw)

	chipKey := "REQ-" + v.run + "-OCC-CHIP"
	chipReuse := v.create(chipKey, chip, "BOARD-"+v.run+"-OCC-NEW")
	v.check(chipReuse.status == http.StatusConflict, "new key reusing a bound chip returns 409 (got %d: %s)", chipReuse.status, chipReuse.raw)
	v.check(chipReuse.errBody.Error.Code == codeDeviceAlreadyBound && chipReuse.errBody.Error.Field == "chip_uid",
		"error is DEVICE_ALREADY_BOUND on chip_uid (got %q field %q)", chipReuse.errBody.Error.Code, chipReuse.errBody.Error.Field)

	boardKey := "REQ-" + v.run + "-OCC-BOARD"
	boardReuse := v.create(boardKey, "CHIP-"+v.run+"-OCC-NEW", board)
	v.check(boardReuse.status == http.StatusConflict, "new key reusing a bound board returns 409 (got %d: %s)", boardReuse.status, boardReuse.raw)
	v.check(boardReuse.errBody.Error.Code == codeDeviceAlreadyBound && boardReuse.errBody.Error.Field == "board_serial",
		"error is DEVICE_ALREADY_BOUND on board_serial (got %q field %q)", boardReuse.errBody.Error.Code, boardReuse.errBody.Error.Field)

	for _, rejected := range []string{chipKey, boardKey} {
		lookup := v.get("/api/v1/bindings/by-request-key/" + rejected)
		v.check(lookup.status == http.StatusNotFound && lookup.errBody.Error.Code == codeNotFound,
			"rejected request key %s is not stored (404)", rejected)
	}
	byChip := v.get("/api/v1/bindings/by-chip-uid/" + chip)
	v.check(byChip.status == http.StatusOK && byChip.binding == first.binding, "chip still resolves to the original pairing")
	byBoard := v.get("/api/v1/bindings/by-board-serial/" + board)
	v.check(byBoard.status == http.StatusOK && byBoard.binding == first.binding, "board still resolves to the original pairing")
}

// scenarioConcurrentStations races two stations on the same identifiers:
// same chip with different request keys, identical retries with the same key,
// and divergent payloads under the same key. Exactly one side may win.
func (v *verifier) scenarioConcurrentStations() {
	fmt.Println("scenario: concurrent stations contend for the same identifiers")

	// Two stations, same chip UID, different request keys and boards.
	chip := "CHIP-" + v.run + "-RACE"
	keyA := "REQ-" + v.run + "-RACE-A"
	keyB := "REQ-" + v.run + "-RACE-B"
	boardA := "BOARD-" + v.run + "-RACE-A"
	boardB := "BOARD-" + v.run + "-RACE-B"
	resps := v.race(
		[3]string{keyA, chip, boardA},
		[3]string{keyB, chip, boardB},
	)

	var winner, loser response
	var winnerKey, loserKey, winnerBoard string
	switch {
	case resps[0].status == http.StatusCreated && resps[1].status == http.StatusConflict:
		winner, loser, winnerKey, loserKey, winnerBoard = resps[0], resps[1], keyA, keyB, boardA
	case resps[1].status == http.StatusCreated && resps[0].status == http.StatusConflict:
		winner, loser, winnerKey, loserKey, winnerBoard = resps[1], resps[0], keyB, keyA, boardB
	default:
		v.check(false, "exactly one station may win the race, got statuses %d and %d", resps[0].status, resps[1].status)
		return
	}
	v.check(true, "exactly one station won the race")
	v.check(loser.errBody.Error.Code == codeDeviceAlreadyBound && loser.errBody.Error.Field == "chip_uid",
		"loser gets DEVICE_ALREADY_BOUND on chip_uid (got %q field %q)", loser.errBody.Error.Code, loser.errBody.Error.Field)

	byChip := v.get("/api/v1/bindings/by-chip-uid/" + chip)
	v.check(byChip.status == http.StatusOK && byChip.binding == winner.binding, "contended chip resolves to the winning record")
	byKey := v.get("/api/v1/bindings/by-request-key/" + winnerKey)
	v.check(byKey.status == http.StatusOK && byKey.binding == winner.binding, "winning request key resolves to the winning record")
	byBoard := v.get("/api/v1/bindings/by-board-serial/" + winnerBoard)
	v.check(byBoard.status == http.StatusOK && byBoard.binding == winner.binding, "winning board resolves to the winning record")
	loserLookup := v.get("/api/v1/bindings/by-request-key/" + loserKey)
	v.check(loserLookup.status == http.StatusNotFound, "losing request key stays unbound (404)")

	// Two stations, same board serial, different request keys and chips.
	board := "BOARD-" + v.run + "-RACEB"
	resps = v.race(
		[3]string{"REQ-" + v.run + "-RACEB-A", "CHIP-" + v.run + "-RACEB-A", board},
		[3]string{"REQ-" + v.run + "-RACEB-B", "CHIP-" + v.run + "-RACEB-B", board},
	)
	created, occupied := 0, 0
	for _, r := range resps {
		switch {
		case r.status == http.StatusCreated:
			created++
		case r.status == http.StatusConflict && r.errBody.Error.Code == codeDeviceAlreadyBound && r.errBody.Error.Field == "board_serial":
			occupied++
		}
	}
	v.check(created == 1 && occupied == 1, "board race yields one 201 and one DEVICE_ALREADY_BOUND/board_serial (got %d/%d)", created, occupied)

	// Two stations, identical retries of the same request key.
	dupKey := "REQ-" + v.run + "-RACEDUP"
	dup := [3]string{dupKey, "CHIP-" + v.run + "-RACEDUP", "BOARD-" + v.run + "-RACEDUP"}
	resps = v.race(dup, dup)
	created, replayed := 0, 0
	for _, r := range resps {
		switch r.status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			replayed++
		}
	}
	v.check(created == 1 && replayed == 1, "racing identical retries yield one 201 and one 200 (got %d/%d)", created, replayed)
	v.check(resps[0].binding == resps[1].binding, "both racing retries observe the same record")

	// Two stations, same request key but divergent payloads.
	confKey := "REQ-" + v.run + "-RACECONF"
	resps = v.race(
		[3]string{confKey, "CHIP-" + v.run + "-RACECONF-A", "BOARD-" + v.run + "-RACECONF-A"},
		[3]string{confKey, "CHIP-" + v.run + "-RACECONF-B", "BOARD-" + v.run + "-RACECONF-B"},
	)
	created, keyConflicts := 0, 0
	for _, r := range resps {
		switch {
		case r.status == http.StatusCreated:
			created++
		case r.status == http.StatusConflict && r.errBody.Error.Code == codeRequestKeyConflict:
			keyConflicts++
		}
	}
	v.check(created == 1 && keyConflicts == 1, "divergent payloads under one key yield one 201 and one REQUEST_KEY_CONFLICT (got %d/%d)", created, keyConflicts)
}

// scenarioValidationRejected confirms invalid identifiers are rejected as a
// whole with 422 and nothing is stored.
func (v *verifier) scenarioValidationRejected() {
	fmt.Println("scenario: invalid identifiers rejected with 422, nothing stored")
	cases := []struct {
		name string
		body string
	}{
		{"lowercase chip uid", `{"request_key":"REQ-` + v.run + `-BAD1","chip_uid":"chip-lower","board_serial":"BOARD-1"}`},
		{"underscore in chip uid", `{"request_key":"REQ-` + v.run + `-BAD2","chip_uid":"CHIP_OK","board_serial":"BOARD-1"}`},
		{"non-ascii board serial", `{"request_key":"REQ-` + v.run + `-BAD3","chip_uid":"CHIP-1","board_serial":"BOARD-é"}`},
		{"empty board serial", `{"request_key":"REQ-` + v.run + `-BAD4","chip_uid":"CHIP-1","board_serial":""}`},
		{"overlong request key", `{"request_key":"` + strings.Repeat("K", 65) + `","chip_uid":"CHIP-1","board_serial":"BOARD-1"}`},
		{"missing board serial", `{"request_key":"REQ-` + v.run + `-BAD6","chip_uid":"CHIP-1"}`},
		{"malformed json", `{"request_key":`},
		{"wrong field type", `{"request_key":"REQ-` + v.run + `-BAD8","chip_uid":42,"board_serial":"BOARD-1"}`},
	}
	for _, tc := range cases {
		resp := v.do(http.MethodPost, "/api/v1/bindings", []byte(tc.body))
		v.check(resp.status == http.StatusUnprocessableEntity && resp.errBody.Error.Code == codeValidationFailed,
			"%s: 422 VALIDATION_FAILED (got %d: %s)", tc.name, resp.status, resp.raw)
	}
	for _, key := range []string{"REQ-" + v.run + "-BAD1", "REQ-" + v.run + "-BAD2", "REQ-" + v.run + "-BAD3", "REQ-" + v.run + "-BAD4", "REQ-" + v.run + "-BAD6", "REQ-" + v.run + "-BAD8"} {
		lookup := v.get("/api/v1/bindings/by-request-key/" + key)
		v.check(lookup.status == http.StatusNotFound, "rejected key %s is not stored", key)
	}
	unknown := v.get("/api/v1/bindings/by-chip-uid/CHIP-" + v.run + "-NOPE")
	v.check(unknown.status == http.StatusNotFound && unknown.errBody.Error.Code == codeNotFound, "unknown chip UID returns 404 NOT_FOUND")
}

// scenarioBatchMixedWithMissing is acceptance path 1: one batch mixes all
// three identifier kinds and includes missing rows; results come back in
// strict input order, each echoing its line and query, hits carry the full
// Binding structure and misses are per-item NOT_FOUND.
func (v *verifier) scenarioBatchMixedWithMissing() {
	fmt.Println("scenario: batch lookup mixes chip UID, board serial and request key with missing rows")
	key := "REQ-" + v.run + "-BMIX"
	chip := "CHIP-" + v.run + "-BMIX"
	board := "BOARD-" + v.run + "-BMIX"

	first := v.create(key, chip, board)
	v.check(first.status == http.StatusCreated, "setup create returns 201 (got %d: %s)", first.status, first.raw)

	// Line numbers are intentionally out of order so the response order can
	// only come from the input order.
	res := v.batch([]batchQuery{
		{Line: 7, Type: "chip_uid", Value: chip},
		{Line: 2, Type: "board_serial", Value: "BOARD-" + v.run + "-MISSING"},
		{Line: 9, Type: "request_key", Value: key},
		{Line: 1, Type: "board_serial", Value: board},
		{Line: 5, Type: "chip_uid", Value: "CHIP-" + v.run + "-MISSING"},
	})
	v.check(res.status == http.StatusOK, "batch returns 200 (got %d: %s)", res.status, res.raw)
	v.check(len(res.results) == 5, "batch returns one result per input row (got %d)", len(res.results))

	want := []struct {
		line   int
		typ    string
		value  string
		status string
	}{
		{7, "chip_uid", chip, "FOUND"},
		{2, "board_serial", "BOARD-" + v.run + "-MISSING", "NOT_FOUND"},
		{9, "request_key", key, "FOUND"},
		{1, "board_serial", board, "FOUND"},
		{5, "chip_uid", "CHIP-" + v.run + "-MISSING", "NOT_FOUND"},
	}
	for i, w := range want {
		if i >= len(res.results) {
			break
		}
		got := res.results[i]
		v.check(got.Line == w.line && got.Type == w.typ && got.Value == w.value,
			"row %d echoes line/type/value (%d/%s/%s)", i, got.Line, got.Type, got.Value)
		v.check(got.Status == w.status, "row %d (line %d) is %s (got %s)", i, w.line, w.status, got.Status)
		if w.status == "FOUND" {
			v.check(got.Binding != nil && *got.Binding == first.binding,
				"row %d hit returns the complete existing Binding", i)
		} else {
			v.check(got.Binding == nil, "row %d miss carries no binding", i)
		}
	}
}

// scenarioBatchDuplicatesReturnedPerRow is acceptance path 2: identical
// conditions are answered on every row, in input order; missing duplicates are
// NOT_FOUND on every row. (Storage deduplicates the identical condition to a
// single database lookup; the batch endpoint tests pin that round-trip
// guarantee.)
func (v *verifier) scenarioBatchDuplicatesReturnedPerRow() {
	fmt.Println("scenario: duplicate batch conditions answered per row")
	key := "REQ-" + v.run + "-BDUP"
	chip := "CHIP-" + v.run + "-BDUP"
	board := "BOARD-" + v.run + "-BDUP"

	first := v.create(key, chip, board)
	v.check(first.status == http.StatusCreated, "setup create returns 201 (got %d: %s)", first.status, first.raw)

	res := v.batch([]batchQuery{
		{Line: 10, Type: "chip_uid", Value: chip},
		{Line: 20, Type: "request_key", Value: "REQ-" + v.run + "-NEVER"},
		{Line: 30, Type: "chip_uid", Value: chip},
		{Line: 40, Type: "chip_uid", Value: chip},
		{Line: 50, Type: "request_key", Value: "REQ-" + v.run + "-NEVER"},
	})
	v.check(res.status == http.StatusOK, "batch returns 200 (got %d: %s)", res.status, res.raw)

	lines := []int{}
	for _, r := range res.results {
		lines = append(lines, r.Line)
	}
	v.check(fmt.Sprint(lines) == "[10 20 30 40 50]", "duplicate rows stay in strict input order (got %v)", lines)
	for _, line := range []int{10, 30, 40} {
		r := res.results[indexByLine(res.results, line)]
		v.check(r.Status == "FOUND" && r.Binding != nil && *r.Binding == first.binding,
			"duplicate hit on line %d returns the single resolved Binding", line)
	}
	for _, line := range []int{20, 50} {
		r := res.results[indexByLine(res.results, line)]
		v.check(r.Status == "NOT_FOUND" && r.Binding == nil,
			"duplicate miss on line %d returns NOT_FOUND with no binding", line)
	}
}

// scenarioBatchSnapshotUnderConcurrentWrites is acceptance path 3: while
// other stations commit new bindings, each batch observes one snapshot — for
// every queried row its request key, chip UID and board serial must be all
// FOUND with one identical record or all NOT_FOUND, never a torn mix.
func (v *verifier) scenarioBatchSnapshotUnderConcurrentWrites() {
	fmt.Println("scenario: batch follows one snapshot while concurrent binds commit")
	const rows = 32 // 32 * 3 identifiers = 96 items, inside the 100-item cap

	// A station continuously commits new bindings.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	writers := 4
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < rows; i += writers {
				select {
				case <-stop:
					return
				default:
				}
				v.create(
					fmt.Sprintf("REQ-%s-SNAP-%02d", v.run, i),
					fmt.Sprintf("CHIP-%s-SNAP-%02d", v.run, i),
					fmt.Sprintf("BOARD-%s-SNAP-%02d", v.run, i),
				)
				time.Sleep(time.Duration(i%3) * time.Millisecond)
			}
		}(w)
	}

	torn := false
	checked := 0
	// Fire batches through the commit window.
	for round := 0; round < 16; round++ {
		time.Sleep(time.Millisecond)
		queries := make([]batchQuery, 0, 3*rows)
		for kind := 0; kind < 3; kind++ {
			for i := 0; i < rows; i++ {
				line := kind*rows + i + 1
				switch kind {
				case 0:
					queries = append(queries, batchQuery{line, "request_key", fmt.Sprintf("REQ-%s-SNAP-%02d", v.run, i)})
				case 1:
					queries = append(queries, batchQuery{line, "chip_uid", fmt.Sprintf("CHIP-%s-SNAP-%02d", v.run, i)})
				default:
					queries = append(queries, batchQuery{line, "board_serial", fmt.Sprintf("BOARD-%s-SNAP-%02d", v.run, i)})
				}
			}
		}
		res := v.batch(queries)
		if res.status != http.StatusOK {
			continue
		}
		byLine := map[int]batchItem{}
		for _, r := range res.results {
			byLine[r.Line] = r
		}
		for i := 0; i < rows; i++ {
			rk := byLine[1+i]
			rc := byLine[rows+1+i]
			rb := byLine[2*rows+1+i]
			found := 0
			for _, r := range []batchItem{rk, rc, rb} {
				if r.Status == "FOUND" {
					found++
				}
			}
			switch found {
			case 3:
				checked++
				same := rk.Binding != nil && rc.Binding != nil && rb.Binding != nil &&
					*rk.Binding == *rc.Binding && *rk.Binding == *rb.Binding
				if !same {
					torn = true
				}
			case 0:
			default:
				torn = true
			}
		}
	}
	close(stop)
	wg.Wait()
	v.check(!torn, "no row mixes FOUND/NOT_FOUND across its three identifiers while writes commit (single snapshot)")
	v.check(checked > 0, "at least one committed row was observed all-three-ways mid-churn (got %d)", checked)

	// Once writers finish, the final batch finds every row all three ways.
	queries := make([]batchQuery, 0, 3*rows)
	for i := 0; i < rows; i++ {
		queries = append(queries,
			batchQuery{3*i + 1, "request_key", fmt.Sprintf("REQ-%s-SNAP-%02d", v.run, i)},
			batchQuery{3*i + 2, "chip_uid", fmt.Sprintf("CHIP-%s-SNAP-%02d", v.run, i)},
			batchQuery{3*i + 3, "board_serial", fmt.Sprintf("BOARD-%s-SNAP-%02d", v.run, i)},
		)
	}
	res := v.batch(queries)
	v.check(res.status == http.StatusOK && len(res.results) == 3*rows, "post-churn batch resolves every row (got %d: %s)", res.status, res.raw)
	missing := 0
	for _, r := range res.results {
		if r.Status != "FOUND" || r.Binding == nil {
			missing++
		}
	}
	v.check(missing == 0, "all %d post-churn items are FOUND with a Binding (missing %d)", 3*rows, missing)
}

// scenarioBatchValidationRejected pins the 422 contract and field positions
// for every invalid batch shape; invalid batches never reach the database.
func (v *verifier) scenarioBatchValidationRejected() {
	fmt.Println("scenario: invalid batch shapes rejected with 422 and field positions")
	cases := []struct {
		name     string
		queries  []batchQuery
		rawBody  string
		location string
	}{
		{"empty array", nil, `{"queries":[]}`, "queries"},
		{"duplicate line numbers", []batchQuery{
			{Line: 3, Type: "chip_uid", Value: "CHIP-X"},
			{Line: 3, Type: "board_serial", Value: "BOARD-X"},
		}, "", "queries[1].line"},
		{"unknown query type", []batchQuery{{Line: 1, Type: "serial", Value: "BOARD-X"}}, "", "queries[0].type"},
		{"illegal identifier", []batchQuery{{Line: 1, Type: "chip_uid", Value: "lowercase"}}, "", "queries[0].value"},
		{"wrong json type for line", nil, `{"queries":[{"line":"1","type":"chip_uid","value":"CHIP-X"}]}`, "queries[0].line"},
	}
	for _, tc := range cases {
		var res batchResponse
		if tc.rawBody != "" {
			res = v.batchRaw(tc.rawBody)
		} else {
			res = v.batch(tc.queries)
		}
		ok := res.status == http.StatusUnprocessableEntity &&
			res.errBody.Error.Code == codeValidationFailed &&
			len(res.errBody.Error.FieldErrors) > 0 &&
			res.errBody.Error.FieldErrors[0].Location == tc.location
		v.check(ok, "%s: 422 VALIDATION_FAILED at %s (got %d: %s)", tc.name, tc.location, res.status, res.raw)
	}
	over := make([]batchQuery, 101)
	for i := range over {
		over[i] = batchQuery{Line: i + 1, Type: "chip_uid", Value: "CHIP-X"}
	}
	res := v.batch(over)
	v.check(res.status == http.StatusUnprocessableEntity && res.errBody.Error.FieldErrors[0].Location == "queries",
		"more than 100 items: 422 at queries (got %d: %s)", res.status, res.raw)
}

// scenarioInspectionVerdicts drives a repair technician's physical
// verification before teardown: the chip and board are scanned together and
// the service must report whether they belong to the same registration
// (CONSISTENT), to two different registrations (MISMATCH), only one side is
// registered (PARTIAL) or neither is (UNREGISTERED). Every verdict is written
// once as an immutable inspection record and re-readable by its id.
func (v *verifier) scenarioInspectionVerdicts() {
	fmt.Println("scenario: physical verification verdicts (consistent/mismatch/partial/unregistered)")

	// Two independent registrations.
	first := v.create("REQ-"+v.run+"-INS-A", "CHIP-"+v.run+"-INS-A", "BOARD-"+v.run+"-INS-A")
	v.check(first.status == http.StatusCreated, "setup binding A returns 201 (got %d: %s)", first.status, first.raw)
	second := v.create("REQ-"+v.run+"-INS-B", "CHIP-"+v.run+"-INS-B", "BOARD-"+v.run+"-INS-B")
	v.check(second.status == http.StatusCreated, "setup binding B returns 201 (got %d: %s)", second.status, second.raw)

	// Same registration on both sides: CONSISTENT, both summaries are binding A.
	consistent := v.inspect("CHIP-"+v.run+"-INS-A", "BOARD-"+v.run+"-INS-A")
	v.check(consistent.status == http.StatusCreated, "paired scan returns 201 (got %d: %s)", consistent.status, consistent.raw)
	v.check(consistent.inspection.Result == "CONSISTENT", "same binding judged CONSISTENT (got %q)", consistent.inspection.Result)
	v.check(consistent.inspection.ChipBindingID != nil && consistent.inspection.BoardBindingID != nil &&
		*consistent.inspection.ChipBindingID == *consistent.inspection.BoardBindingID &&
		*consistent.inspection.ChipBindingID == first.binding.BindingID,
		"CONSISTENT record references the one shared binding id")
	v.check(consistent.inspection.ChipBinding != nil && *consistent.inspection.ChipBinding == first.binding &&
		consistent.inspection.BoardBinding != nil && *consistent.inspection.BoardBinding == first.binding,
		"CONSISTENT response carries the matched binding summary on both sides")

	// Chip from binding A with board from binding B: MISMATCH.
	mismatch := v.inspect("CHIP-"+v.run+"-INS-A", "BOARD-"+v.run+"-INS-B")
	v.check(mismatch.status == http.StatusCreated, "cross scan returns 201 (got %d: %s)", mismatch.status, mismatch.raw)
	v.check(mismatch.inspection.Result == "MISMATCH", "cross bindings judged MISMATCH (got %q)", mismatch.inspection.Result)
	v.check(mismatch.inspection.ChipBindingID != nil && mismatch.inspection.BoardBindingID != nil &&
		*mismatch.inspection.ChipBindingID == first.binding.BindingID &&
		*mismatch.inspection.BoardBindingID == second.binding.BindingID,
		"MISMATCH record keeps each side's own binding id")
	v.check(mismatch.inspection.ChipBinding != nil && *mismatch.inspection.ChipBinding == first.binding &&
		mismatch.inspection.BoardBinding != nil && *mismatch.inspection.BoardBinding == second.binding,
		"MISMATCH response carries each side's own binding summary")

	// Only one side registered: PARTIAL, in both directions.
	chipOnly := v.inspect("CHIP-"+v.run+"-INS-A", "BOARD-"+v.run+"-INS-MISSING")
	v.check(chipOnly.inspection.Result == "PARTIAL" && chipOnly.status == http.StatusCreated,
		"registered chip with unregistered board judged PARTIAL (got %d/%q)", chipOnly.status, chipOnly.inspection.Result)
	v.check(chipOnly.inspection.ChipBindingID != nil && *chipOnly.inspection.ChipBindingID == first.binding.BindingID &&
		chipOnly.inspection.BoardBindingID == nil && chipOnly.inspection.BoardBinding == nil,
		"chip-side PARTIAL carries only the chip binding")
	boardOnly := v.inspect("CHIP-"+v.run+"-INS-MISSING", "BOARD-"+v.run+"-INS-B")
	v.check(boardOnly.inspection.Result == "PARTIAL" && boardOnly.status == http.StatusCreated,
		"unregistered chip with registered board judged PARTIAL (got %d/%q)", boardOnly.status, boardOnly.inspection.Result)
	v.check(boardOnly.inspection.BoardBindingID != nil && *boardOnly.inspection.BoardBindingID == second.binding.BindingID &&
		boardOnly.inspection.ChipBindingID == nil && boardOnly.inspection.ChipBinding == nil,
		"board-side PARTIAL carries only the board binding")

	// Neither side registered: still an auditable record, UNREGISTERED.
	unregistered := v.inspect("CHIP-"+v.run+"-INS-GHOST", "BOARD-"+v.run+"-INS-GHOST")
	v.check(unregistered.status == http.StatusCreated && unregistered.inspection.Result == "UNREGISTERED",
		"two unregistered identifiers judged UNREGISTERED (got %d/%q)", unregistered.status, unregistered.inspection.Result)
	v.check(unregistered.inspection.ChipBindingID == nil && unregistered.inspection.BoardBindingID == nil &&
		unregistered.inspection.ChipBinding == nil && unregistered.inspection.BoardBinding == nil,
		"UNREGISTERED record carries no binding references")

	// Every verdict is persistent and comes back unchanged.
	for name, res := range map[string]inspectionResponse{
		"consistent":    consistent,
		"mismatch":      mismatch,
		"chip partial":  chipOnly,
		"board partial": boardOnly,
		"unregistered":  unregistered,
	} {
		got := v.getInspection(res.inspection.InspectionID)
		v.check(got.status == http.StatusOK && inspectionsEqual(got.inspection, res.inspection),
			"%s inspection %d is re-readable with the original verdict and summaries", name, res.inspection.InspectionID)
	}
}

// scenarioInspectionReviewAndErrors covers the review endpoint's error
// contract and the immutability of stored inspections: invalid path ids and
// invalid scan payloads are rejected as validation errors without writing a
// record, legal but unknown ids are 404, and inspection creation never alters
// the binding ledger.
func (v *verifier) scenarioInspectionReviewAndErrors() {
	fmt.Println("scenario: inspection review, validation and immutability")

	first := v.create("REQ-"+v.run+"-INR", "CHIP-"+v.run+"-INR", "BOARD-"+v.run+"-INR")
	v.check(first.status == http.StatusCreated, "setup binding returns 201 (got %d: %s)", first.status, first.raw)

	created := v.inspect("CHIP-"+v.run+"-INR", "BOARD-"+v.run+"-INR")
	v.check(created.status == http.StatusCreated && created.inspection.Result == "CONSISTENT",
		"setup inspection is CONSISTENT (got %d: %s)", created.status, created.raw)

	// Non-positive-integer path ids are validation errors, not 404s.
	for _, bad := range []string{"0", "-1", "abc", "1.5", "+1", "01"} {
		res := v.get("/api/v1/inspections/" + bad)
		v.check(res.status == http.StatusUnprocessableEntity && res.errBody.Error.Code == codeValidationFailed,
			"path id %q rejected with 422 VALIDATION_FAILED (got %d: %s)", bad, res.status, res.raw)
	}

	// A legal id without a record is the ordinary 404.
	missing := v.getInspection(999999999)
	v.check(missing.status == http.StatusNotFound && missing.errBody.Error.Code == codeNotFound,
		"legal but unknown inspection id returns 404 NOT_FOUND (got %d: %s)", missing.status, missing.raw)

	// Invalid scan payloads are 422 and must not create records.
	badBodies := []string{
		`{"chip_uid":"chip-lower","board_serial":"BOARD-1"}`,
		`{"chip_uid":"CHIP_1","board_serial":"BOARD-1"}`,
		`{"chip_uid":"CHIP-1","board_serial":""}`,
		`{"chip_uid":"CHIP-1"}`,
		`{}`,
		`{"chip_uid":7,"board_serial":"BOARD-1"}`,
		`{"chip_uid":"CHIP-1","board_serial":"BOARD-1","extra":true}`,
		`{`,
	}
	for _, body := range badBodies {
		res := v.inspectRaw(body)
		v.check(res.status == http.StatusUnprocessableEntity && res.errBody.Error.Code == codeValidationFailed,
			"invalid scan payload rejected with 422 (got %d: %s)", res.status, res.raw)
	}

	// The stored record is unchanged by all the rejected attempts, and the
	// binding ledger is untouched by inspection activity (one setup binding).
	review := v.getInspection(created.inspection.InspectionID)
	v.check(review.status == http.StatusOK && inspectionsEqual(review.inspection, created.inspection),
		"recorded inspection remains unchanged and reviewable")
	stillBound := v.get("/api/v1/bindings/by-chip-uid/CHIP-" + v.run + "-INR")
	v.check(stillBound.status == http.StatusOK && stillBound.binding == first.binding,
		"inspections do not modify existing bindings")
}

// scenarioMismatchPeers drives the repair supervisor's triage view: a binding
// suspected of a mix-up reveals every other binding it was implicated with in
// mismatch inspections, most repeated first, with the co-occurrence count,
// the most recent inspection id and its timestamp. Both sides of one relation
// must agree on the count, ties must order deterministically, the limit must
// clip the list, and consistent or partial verifications must not feed the
// statistics.
func (v *verifier) scenarioMismatchPeers() {
	fmt.Println("scenario: mismatch peers triage view")

	mk := func(tag string) response {
		r := v.create("REQ-"+v.run+"-MP-"+tag, "CHIP-"+v.run+"-MP-"+tag, "BOARD-"+v.run+"-MP-"+tag)
		v.check(r.status == http.StatusCreated, "setup binding %s returns 201 (got %d: %s)", tag, r.status, r.raw)
		return r
	}
	a := mk("A")
	b := mk("B")
	c := mk("C")
	d := mk("D")

	// A-B mismatch twice (both scan directions), A-C and A-D once each.
	v.inspect("CHIP-"+v.run+"-MP-A", "BOARD-"+v.run+"-MP-B")
	mAB2 := v.inspect("CHIP-"+v.run+"-MP-B", "BOARD-"+v.run+"-MP-A")
	mAC := v.inspect("CHIP-"+v.run+"-MP-A", "BOARD-"+v.run+"-MP-C")
	mAD := v.inspect("CHIP-"+v.run+"-MP-A", "BOARD-"+v.run+"-MP-D")
	v.check(mAB2.inspection.Result == "MISMATCH" && mAC.inspection.Result == "MISMATCH" && mAD.inspection.Result == "MISMATCH",
		"setup cross scans are MISMATCH")

	// Consistent and partial verifications must not feed the statistics.
	v.inspect("CHIP-"+v.run+"-MP-A", "BOARD-"+v.run+"-MP-A")
	v.inspect("CHIP-"+v.run+"-MP-A", "BOARD-"+v.run+"-MP-GHOST")

	res := v.mismatchPeers(a.binding.BindingID, "")
	v.check(res.status == http.StatusOK, "mismatch-peers returns 200 (got %d: %s)", res.status, res.raw)
	v.check(res.body.Binding == a.binding, "response carries the target binding summary")
	ok := len(res.body.Peers) == 3
	v.check(ok, "three implicated peers returned, consistent/partial scans excluded (got %d)", len(res.body.Peers))
	if ok {
		v.check(res.body.Peers[0].Binding == b.binding && res.body.Peers[0].Occurrences == 2,
			"most repeated peer first: B with 2 occurrences (got binding %d, %d)",
			res.body.Peers[0].Binding.BindingID, res.body.Peers[0].Occurrences)
		v.check(res.body.Peers[0].LastInspectionID == mAB2.inspection.InspectionID &&
			res.body.Peers[0].LastOccurredAt == mAB2.inspection.CreatedAt,
			"peer B pins the most recent of its two inspections")
		// A-C and A-D tie at one occurrence: most recent inspection first.
		v.check(res.body.Peers[1].Binding == d.binding && res.body.Peers[2].Binding == c.binding,
			"equal-count peers order by most recent inspection first")
		v.check(res.body.Peers[1].LastInspectionID == mAD.inspection.InspectionID &&
			res.body.Peers[2].LastInspectionID == mAC.inspection.InspectionID,
			"tied peers pin their own latest inspections")
	}

	// Both sides of the same relation agree on count and latest inspection.
	rev := v.mismatchPeers(b.binding.BindingID, "")
	v.check(rev.status == http.StatusOK && len(rev.body.Peers) == 1 &&
		rev.body.Peers[0].Binding == a.binding && rev.body.Peers[0].Occurrences == 2 &&
		rev.body.Peers[0].LastInspectionID == mAB2.inspection.InspectionID,
		"viewed from B the same relation shows identical count and latest inspection (got %d: %s)", rev.status, rev.raw)

	// The limit clips the deterministic ordering.
	lim := v.mismatchPeers(a.binding.BindingID, "?limit=1")
	v.check(lim.status == http.StatusOK && len(lim.body.Peers) == 1 && lim.body.Peers[0].Binding == b.binding,
		"limit=1 keeps only the most repeated peer (got %d: %s)", lim.status, lim.raw)
	lim2 := v.mismatchPeers(a.binding.BindingID, "?limit=2")
	v.check(lim2.status == http.StatusOK && len(lim2.body.Peers) == 2,
		"limit=2 keeps the two most relevant peers (got %d)", len(lim2.body.Peers))

	// A binding with no mismatch history returns an empty array.
	e := mk("E")
	empty := v.mismatchPeers(e.binding.BindingID, "")
	v.check(empty.status == http.StatusOK && len(empty.body.Peers) == 0 && strings.Contains(string(empty.raw), `"peers":[]`),
		"binding without mismatches returns an empty peers array (got %d: %s)", empty.status, empty.raw)

	// Illegal ids and limits are 422 before any query; unknown ids are 404.
	for _, bad := range []string{"0", "-1", "abc", "01"} {
		r := v.mismatchPeersRaw("/api/v1/bindings/" + bad + "/mismatch-peers")
		v.check(r.status == http.StatusUnprocessableEntity && r.errBody.Error.Code == codeValidationFailed,
			"binding id %q rejected with 422 VALIDATION_FAILED (got %d: %s)", bad, r.status, r.raw)
	}
	for _, bad := range []string{"0", "51", "abc"} {
		r := v.mismatchPeers(a.binding.BindingID, "?limit="+bad)
		v.check(r.status == http.StatusUnprocessableEntity && r.errBody.Error.Code == codeValidationFailed,
			"limit %q rejected with 422 VALIDATION_FAILED (got %d: %s)", bad, r.status, r.raw)
	}
	missing := v.mismatchPeers(999999999, "")
	v.check(missing.status == http.StatusNotFound && missing.errBody.Error.Code == codeNotFound,
		"unknown binding id returns 404 NOT_FOUND (got %d: %s)", missing.status, missing.raw)

	// The existing endpoints are untouched by the new route.
	byChip := v.get("/api/v1/bindings/by-chip-uid/CHIP-" + v.run + "-MP-A")
	v.check(byChip.status == http.StatusOK && byChip.binding == a.binding,
		"binding lookup unchanged alongside mismatch-peers")
	review := v.getInspection(mAB2.inspection.InspectionID)
	v.check(review.status == http.StatusOK && inspectionsEqual(review.inspection, mAB2.inspection),
		"inspection review unchanged alongside mismatch-peers")
}

// inspectionsEqual compares two inspection records deeply: the embedded
// binding summaries are pointers, so a plain == would compare addresses even
// when the payloads are byte-identical.
func inspectionsEqual(a, b inspection) bool {
	if a.InspectionID != b.InspectionID || a.ChipUID != b.ChipUID ||
		a.BoardSerial != b.BoardSerial || a.Result != b.Result || a.CreatedAt != b.CreatedAt {
		return false
	}
	if (a.ChipBindingID == nil) != (b.ChipBindingID == nil) {
		return false
	}
	if a.ChipBindingID != nil && *a.ChipBindingID != *b.ChipBindingID {
		return false
	}
	if (a.BoardBindingID == nil) != (b.BoardBindingID == nil) {
		return false
	}
	if a.BoardBindingID != nil && *a.BoardBindingID != *b.BoardBindingID {
		return false
	}
	if (a.ChipBinding == nil) != (b.ChipBinding == nil) {
		return false
	}
	if a.ChipBinding != nil && *a.ChipBinding != *b.ChipBinding {
		return false
	}
	if (a.BoardBinding == nil) != (b.BoardBinding == nil) {
		return false
	}
	if a.BoardBinding != nil && *a.BoardBinding != *b.BoardBinding {
		return false
	}
	return true
}

func indexByLine(items []batchItem, line int) int {
	for i := range items {
		if items[i].Line == line {
			return i
		}
	}
	return -1
}

// race fires one request per payload from separate goroutines released at the
// same instant, simulating stations on the line firing concurrently.
func (v *verifier) race(payloads ...[3]string) []response {
	start := make(chan struct{})
	resps := make([]response, len(payloads))
	var wg sync.WaitGroup
	for i, p := range payloads {
		wg.Add(1)
		go func(i int, p [3]string) {
			defer wg.Done()
			<-start
			resps[i] = v.create(p[0], p[1], p[2])
		}(i, p)
	}
	close(start)
	wg.Wait()
	return resps
}

func (v *verifier) check(ok bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ok {
		fmt.Printf("  PASS %s\n", msg)
		return
	}
	v.failures++
	fmt.Printf("  FAIL %s\n", msg)
}

func (v *verifier) waitHealthy(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		resp, err := v.client.Get(v.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s/healthz", v.base)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (v *verifier) create(key, chip, board string) response {
	body, _ := json.Marshal(map[string]string{
		"request_key":  key,
		"chip_uid":     chip,
		"board_serial": board,
	})
	return v.do(http.MethodPost, "/api/v1/bindings", body)
}

func (v *verifier) get(path string) response {
	return v.do(http.MethodGet, path, nil)
}

// inspect posts one physical verification (a chip and board scanned
// together) and parses either the inspection record or the error envelope.
func (v *verifier) inspect(chip, board string) inspectionResponse {
	body, _ := json.Marshal(map[string]string{
		"chip_uid":     chip,
		"board_serial": board,
	})
	return v.inspectRaw(string(body))
}

// inspectRaw posts a raw inspection body without failing the run, so malformed
// payloads can be tested too.
func (v *verifier) inspectRaw(body string) inspectionResponse {
	req, err := http.NewRequest(http.MethodPost, v.base+"/api/v1/inspections", strings.NewReader(body))
	if err != nil {
		return inspectionResponse{status: -1, raw: []byte(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return inspectionResponse{status: -1, raw: []byte(err.Error())}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := inspectionResponse{status: resp.StatusCode, raw: raw}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		_ = json.Unmarshal(raw, &out.inspection)
	} else {
		_ = json.Unmarshal(raw, &out.errBody)
	}
	return out
}

// getInspection fetches one recorded inspection for technician review.
func (v *verifier) getInspection(id int64) inspectionResponse {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/inspections/%d", v.base, id), nil)
	if err != nil {
		return inspectionResponse{status: -1, raw: []byte(err.Error())}
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return inspectionResponse{status: -1, raw: []byte(err.Error())}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := inspectionResponse{status: resp.StatusCode, raw: raw}
	if resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(raw, &out.inspection)
	} else {
		_ = json.Unmarshal(raw, &out.errBody)
	}
	return out
}

// mismatchPeers queries the mismatch-peers endpoint for one binding id; the
// limitQuery carries an optional raw query string such as "?limit=5".
func (v *verifier) mismatchPeers(id int64, limitQuery string) mismatchPeersResponse {
	return v.mismatchPeersRaw(fmt.Sprintf("/api/v1/bindings/%d/mismatch-peers%s", id, limitQuery))
}

// mismatchPeersRaw fetches a mismatch-peers path verbatim, so illegal ids and
// limits can be probed too.
func (v *verifier) mismatchPeersRaw(path string) mismatchPeersResponse {
	req, err := http.NewRequest(http.MethodGet, v.base+path, nil)
	if err != nil {
		return mismatchPeersResponse{status: -1, raw: []byte(err.Error())}
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return mismatchPeersResponse{status: -1, raw: []byte(err.Error())}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := mismatchPeersResponse{status: resp.StatusCode, raw: raw}
	if resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(raw, &out.body)
	} else {
		_ = json.Unmarshal(raw, &out.errBody)
	}
	return out
}

// batch posts one batch lookup request and parses either the result list or
// the error envelope.
func (v *verifier) batch(queries []batchQuery) batchResponse {
	body, _ := json.Marshal(batchRequest{Queries: queries})
	return v.batchRaw(string(body))
}

func (v *verifier) batchRaw(body string) batchResponse {
	req, err := http.NewRequest(http.MethodPost, v.base+"/api/v1/bindings/batch-lookup", strings.NewReader(body))
	if err != nil {
		return batchResponse{status: -1, raw: []byte(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return batchResponse{status: -1, raw: []byte(err.Error())}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := batchResponse{status: resp.StatusCode, raw: raw}
	if resp.StatusCode == http.StatusOK {
		var parsed batchResponseBody
		_ = json.Unmarshal(raw, &parsed)
		out.results = parsed.Results
	} else {
		_ = json.Unmarshal(raw, &out.errBody)
	}
	return out
}

func (v *verifier) do(method, path string, body []byte) response {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, v.base+path, rdr)
	if err != nil {
		return response{status: -1, raw: []byte(err.Error())}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return response{status: -1, raw: []byte(err.Error())}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, raw: raw}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		_ = json.Unmarshal(raw, &out.binding)
	} else {
		_ = json.Unmarshal(raw, &out.errBody)
	}
	return out
}
