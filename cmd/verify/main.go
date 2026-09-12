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
)

type binding struct {
	BindingID   int64  `json:"binding_id"`
	RequestKey  string `json:"request_key"`
	ChipUID     string `json:"chip_uid"`
	BoardSerial string `json:"board_serial"`
	CreatedAt   string `json:"created_at"`
}

type errorEnvelope struct {
	Error struct {
		Code  string `json:"code"`
		Field string `json:"field"`
	} `json:"error"`
}

type response struct {
	status  int
	binding binding
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
