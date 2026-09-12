package binding_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/binding-service/internal/binding"
	"github.com/example/binding-service/internal/db"
)

// These integration tests run against a real PostgreSQL database (set
// TEST_DATABASE_URL, e.g. postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable).
// They cover the acceptance-critical behaviors: concurrency, replay, and
// persistence across a service restart.

type bindingJSON struct {
	BindingID   int64  `json:"binding_id"`
	RequestKey  string `json:"request_key"`
	ChipUID     string `json:"chip_uid"`
	BoardSerial string `json:"board_serial"`
	CreatedAt   string `json:"created_at"`
}

type errorJSON struct {
	Error struct {
		Code  string `json:"code"`
		Field string `json:"field"`
	} `json:"error"`
}

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return dsn
}

// startApp boots a full service instance (pool, migrations, HTTP server) the
// same way cmd/server does.
func startApp(t *testing.T, dsn string) (*pgxpool.Pool, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	pool, err := db.NewPool(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, db.Migrate(ctx, pool))
	srv := httptest.NewServer(binding.NewRouter(binding.NewHandler(binding.NewStore(pool))))
	return pool, srv
}

type testEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	server *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	pool, srv := startApp(t, testDSN(t))
	_, err := pool.Exec(context.Background(), `TRUNCATE requests, bindings`)
	require.NoError(t, err)
	t.Cleanup(func() {
		srv.Close()
		pool.Close()
	})
	return &testEnv{t: t, pool: pool, server: srv}
}

func (e *testEnv) createRaw(key, chip, board string) (int, []byte, error) {
	body := fmt.Sprintf(`{"request_key":%q,"chip_uid":%q,"board_serial":%q}`, key, chip, board)
	resp, err := http.Post(e.server.URL+"/api/v1/bindings", "application/json", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

func (e *testEnv) mustCreate(t *testing.T, key, chip, board string) (int, []byte) {
	t.Helper()
	status, raw, err := e.createRaw(key, chip, board)
	require.NoError(t, err)
	return status, raw
}

func (e *testEnv) postBody(t *testing.T, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(e.server.URL+"/api/v1/bindings", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func (e *testEnv) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(e.server.URL + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func (e *testEnv) assertCounts(t *testing.T, requests, bindings int64) {
	t.Helper()
	var rc, bc int64
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM requests`).Scan(&rc))
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM bindings`).Scan(&bc))
	assert.Equal(t, requests, rc, "request ledger rows")
	assert.Equal(t, bindings, bc, "binding rows")
}

func parseBinding(t *testing.T, raw []byte) bindingJSON {
	t.Helper()
	var b bindingJSON
	require.NoError(t, json.Unmarshal(raw, &b))
	return b
}

func parseError(t *testing.T, raw []byte) errorJSON {
	t.Helper()
	var e errorJSON
	require.NoError(t, json.Unmarshal(raw, &e))
	return e
}

func TestCreateAndQuery(t *testing.T) {
	env := newTestEnv(t)

	status, raw := env.mustCreate(t, "REQ-1", "CHIP-1", "BOARD-1")
	require.Equal(t, http.StatusCreated, status)
	created := parseBinding(t, raw)
	assert.Equal(t, "REQ-1", created.RequestKey)
	assert.Equal(t, "CHIP-1", created.ChipUID)
	assert.Equal(t, "BOARD-1", created.BoardSerial)
	assert.Positive(t, created.BindingID)
	assert.NotEmpty(t, created.CreatedAt)

	for _, path := range []string{
		"/api/v1/bindings/by-request-key/REQ-1",
		"/api/v1/bindings/by-chip-uid/CHIP-1",
		"/api/v1/bindings/by-board-serial/BOARD-1",
	} {
		status, raw := env.get(t, path)
		require.Equal(t, http.StatusOK, status, path)
		assert.Equal(t, created, parseBinding(t, raw), path)
	}

	for _, path := range []string{
		"/api/v1/bindings/by-request-key/REQ-UNKNOWN",
		"/api/v1/bindings/by-chip-uid/CHIP-UNKNOWN",
		"/api/v1/bindings/by-board-serial/BOARD-UNKNOWN",
	} {
		status, raw := env.get(t, path)
		require.Equal(t, http.StatusNotFound, status, path)
		assert.Equal(t, "NOT_FOUND", parseError(t, raw).Error.Code, path)
	}

	status, _ = env.get(t, "/healthz")
	assert.Equal(t, http.StatusOK, status)
}

func TestValidationRejected(t *testing.T) {
	env := newTestEnv(t)

	bodies := map[string]string{
		"lowercase chip uid":     `{"request_key":"REQ-V1","chip_uid":"chip-1","board_serial":"BOARD-1"}`,
		"underscore in chip uid": `{"request_key":"REQ-V2","chip_uid":"CHIP_1","board_serial":"BOARD-1"}`,
		"non-ascii board serial": `{"request_key":"REQ-V3","chip_uid":"CHIP-1","board_serial":"BOARD-é"}`,
		"empty board serial":     `{"request_key":"REQ-V4","chip_uid":"CHIP-1","board_serial":""}`,
		"overlong request key":   `{"request_key":"` + strings.Repeat("K", 65) + `","chip_uid":"CHIP-1","board_serial":"BOARD-1"}`,
		"missing board serial":   `{"request_key":"REQ-V6","chip_uid":"CHIP-1"}`,
		"malformed json":         `{"request_key":`,
		"wrong field type":       `{"request_key":"REQ-V8","chip_uid":42,"board_serial":"BOARD-1"}`,
		"unknown field":          `{"request_key":"REQ-V9","chip_uid":"CHIP-1","board_serial":"BOARD-1","extra":"x"}`,
		"empty body":             ``,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			status, raw := env.postBody(t, body)
			require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
			assert.Equal(t, "VALIDATION_FAILED", parseError(t, raw).Error.Code)
		})
	}

	// The whole request is rejected: nothing reaches the database.
	env.assertCounts(t, 0, 0)
}

func TestReplayReturnsOriginal(t *testing.T) {
	env := newTestEnv(t)

	status, first := env.mustCreate(t, "REQ-REPLAY", "CHIP-REPLAY", "BOARD-REPLAY")
	require.Equal(t, http.StatusCreated, status)

	// The station never saw the first response and re-sends the same payload.
	status, replay := env.mustCreate(t, "REQ-REPLAY", "CHIP-REPLAY", "BOARD-REPLAY")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, string(first), string(replay), "replay returns the original result byte-for-byte")
	assert.Equal(t, parseBinding(t, first), parseBinding(t, replay))

	env.assertCounts(t, 1, 1)
}

func TestRequestKeyConflict(t *testing.T) {
	env := newTestEnv(t)

	status, first := env.mustCreate(t, "REQ-KEYCONF", "CHIP-KEYCONF", "BOARD-KEYCONF")
	require.Equal(t, http.StatusCreated, status)

	status, raw := env.mustCreate(t, "REQ-KEYCONF", "CHIP-OTHER", "BOARD-OTHER")
	require.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "REQUEST_KEY_CONFLICT", parseError(t, raw).Error.Code)
	// The conflict response carries the pre-existing record for reconciliation.
	var body struct {
		Existing bindingJSON `json:"existing"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, parseBinding(t, first), body.Existing)

	// The original record is untouched.
	status, raw = env.get(t, "/api/v1/bindings/by-request-key/REQ-KEYCONF")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, parseBinding(t, first), parseBinding(t, raw))
	env.assertCounts(t, 1, 1)
}

func TestDeviceOccupied(t *testing.T) {
	env := newTestEnv(t)

	status, first := env.mustCreate(t, "REQ-OCC", "CHIP-OCC", "BOARD-OCC")
	require.Equal(t, http.StatusCreated, status)

	// A different request key reusing the bound chip UID.
	status, raw := env.mustCreate(t, "REQ-OCC-CHIP", "CHIP-OCC", "BOARD-NEW")
	require.Equal(t, http.StatusConflict, status)
	errBody := parseError(t, raw)
	assert.Equal(t, "DEVICE_ALREADY_BOUND", errBody.Error.Code)
	assert.Equal(t, "chip_uid", errBody.Error.Field)

	// A different request key reusing the bound board serial.
	status, raw = env.mustCreate(t, "REQ-OCC-BOARD", "CHIP-NEW", "BOARD-OCC")
	require.Equal(t, http.StatusConflict, status)
	errBody = parseError(t, raw)
	assert.Equal(t, "DEVICE_ALREADY_BOUND", errBody.Error.Code)
	assert.Equal(t, "board_serial", errBody.Error.Field)

	// Rejected attempts leave no ledger entries behind.
	for _, key := range []string{"REQ-OCC-CHIP", "REQ-OCC-BOARD"} {
		status, _ = env.get(t, "/api/v1/bindings/by-request-key/"+key)
		assert.Equal(t, http.StatusNotFound, status, key)
	}

	// Both devices still resolve to the one original pairing.
	for _, path := range []string{
		"/api/v1/bindings/by-chip-uid/CHIP-OCC",
		"/api/v1/bindings/by-board-serial/BOARD-OCC",
	} {
		status, raw = env.get(t, path)
		require.Equal(t, http.StatusOK, status, path)
		assert.Equal(t, parseBinding(t, first), parseBinding(t, raw), path)
	}
	env.assertCounts(t, 1, 1)
}

// raceResult collects the outcome of one concurrent create attempt.
type raceResult struct {
	status int
	raw    []byte
	err    error
}

// raceCreates fires n concurrent create requests released at the same
// instant, simulating stations (or retries) racing on the line.
func raceCreates(t *testing.T, env *testEnv, payloads [][3]string) []raceResult {
	t.Helper()
	results := make([]raceResult, len(payloads))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, p := range payloads {
		wg.Add(1)
		go func(i int, p [3]string) {
			defer wg.Done()
			<-start
			status, raw, err := env.createRaw(p[0], p[1], p[2])
			results[i] = raceResult{status: status, raw: raw, err: err}
		}(i, p)
	}
	close(start)
	wg.Wait()
	for i, r := range results {
		require.NoError(t, r.err, "request %d failed", i)
	}
	return results
}

func identicalPayloads(n int, key, chip, board string) [][3]string {
	payloads := make([][3]string, n)
	for i := range payloads {
		payloads[i] = [3]string{key, chip, board}
	}
	return payloads
}

func TestConcurrentIdenticalRequests(t *testing.T) {
	env := newTestEnv(t)
	const n = 16

	results := raceCreates(t, env, identicalPayloads(n, "REQ-CONC-ID", "CHIP-CONC-ID", "BOARD-CONC-ID"))

	created, replayed := 0, 0
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			replayed++
		default:
			t.Fatalf("unexpected status %d: %s", r.status, r.raw)
		}
	}
	assert.Equal(t, 1, created, "exactly one of the racing requests may create the record")
	assert.Equal(t, n-1, replayed, "every replay observes the original record")
	for i, r := range results {
		assert.Equal(t, string(results[0].raw), string(r.raw), "response %d carries the same record", i)
	}
	env.assertCounts(t, 1, 1)
}

func TestConcurrentSameChip(t *testing.T) {
	env := newTestEnv(t)
	const n = 16

	payloads := make([][3]string, n)
	for i := range payloads {
		payloads[i] = [3]string{fmt.Sprintf("REQ-CONC-CHIP-%02d", i), "CHIP-CONC-SHARED", fmt.Sprintf("BOARD-CONC-CHIP-%02d", i)}
	}
	results := raceCreates(t, env, payloads)

	created, occupied := 0, 0
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			errBody := parseError(t, r.raw)
			require.Equal(t, "DEVICE_ALREADY_BOUND", errBody.Error.Code, string(r.raw))
			require.Equal(t, "chip_uid", errBody.Error.Field, string(r.raw))
			occupied++
		default:
			t.Fatalf("unexpected status %d: %s", r.status, r.raw)
		}
	}
	assert.Equal(t, 1, created, "exactly one station may bind the shared chip")
	assert.Equal(t, n-1, occupied, "all other stations learn the chip is occupied")

	// The shared chip resolves to the single winning record.
	status, raw := env.get(t, "/api/v1/bindings/by-chip-uid/CHIP-CONC-SHARED")
	require.Equal(t, http.StatusOK, status)
	winner := parseBinding(t, raw)
	assert.Equal(t, "CHIP-CONC-SHARED", winner.ChipUID)
	env.assertCounts(t, 1, 1)
}

func TestConcurrentSameBoard(t *testing.T) {
	env := newTestEnv(t)
	const n = 16

	payloads := make([][3]string, n)
	for i := range payloads {
		payloads[i] = [3]string{fmt.Sprintf("REQ-CONC-BOARD-%02d", i), fmt.Sprintf("CHIP-CONC-BOARD-%02d", i), "BOARD-CONC-SHARED"}
	}
	results := raceCreates(t, env, payloads)

	created, occupied := 0, 0
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			errBody := parseError(t, r.raw)
			require.Equal(t, "DEVICE_ALREADY_BOUND", errBody.Error.Code, string(r.raw))
			require.Equal(t, "board_serial", errBody.Error.Field, string(r.raw))
			occupied++
		default:
			t.Fatalf("unexpected status %d: %s", r.status, r.raw)
		}
	}
	assert.Equal(t, 1, created, "exactly one station may bind the shared board")
	assert.Equal(t, n-1, occupied)
	env.assertCounts(t, 1, 1)
}

func TestConcurrentSameKeyDifferentPayloads(t *testing.T) {
	env := newTestEnv(t)
	const n = 16

	payloads := make([][3]string, n)
	for i := range payloads {
		payloads[i] = [3]string{"REQ-CONC-DIVERGE", fmt.Sprintf("CHIP-CONC-DIVERGE-%02d", i), fmt.Sprintf("BOARD-CONC-DIVERGE-%02d", i)}
	}
	results := raceCreates(t, env, payloads)

	created, conflicts := 0, 0
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			require.Equal(t, "REQUEST_KEY_CONFLICT", parseError(t, r.raw).Error.Code, string(r.raw))
			conflicts++
		default:
			t.Fatalf("unexpected status %d: %s", r.status, r.raw)
		}
	}
	assert.Equal(t, 1, created, "exactly one payload may win the request key")
	assert.Equal(t, n-1, conflicts, "divergent losers get a distinguishable request-key conflict")
	env.assertCounts(t, 1, 1)
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	// First incarnation of the service: create a binding, then shut down.
	pool1, srv1 := startApp(t, dsn)
	_, err := pool1.Exec(ctx, `TRUNCATE requests, bindings`)
	require.NoError(t, err)
	env1 := &testEnv{t: t, pool: pool1, server: srv1}

	status, first := env1.mustCreate(t, "REQ-RESTART", "CHIP-RESTART", "BOARD-RESTART")
	require.Equal(t, http.StatusCreated, status)
	srv1.Close()
	pool1.Close()

	// Second incarnation: new process state, new connection pool, same database.
	pool2, srv2 := startApp(t, dsn)
	defer srv2.Close()
	defer pool2.Close()
	env2 := &testEnv{t: t, pool: pool2, server: srv2}

	for _, path := range []string{
		"/api/v1/bindings/by-request-key/REQ-RESTART",
		"/api/v1/bindings/by-chip-uid/CHIP-RESTART",
		"/api/v1/bindings/by-board-serial/BOARD-RESTART",
	} {
		status, raw := env2.get(t, path)
		require.Equal(t, http.StatusOK, status, path)
		assert.Equal(t, parseBinding(t, first), parseBinding(t, raw), "record survives a service restart: %s", path)
	}

	// A replay after the restart still returns the original result.
	status, replay := env2.mustCreate(t, "REQ-RESTART", "CHIP-RESTART", "BOARD-RESTART")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, string(first), string(replay))

	// And the one-to-one rules are still enforced after the restart.
	status, raw := env2.mustCreate(t, "REQ-RESTART-2", "CHIP-RESTART", "BOARD-RESTART-2")
	require.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "DEVICE_ALREADY_BOUND", parseError(t, raw).Error.Code)
	env2.assertCounts(t, 1, 1)
}
