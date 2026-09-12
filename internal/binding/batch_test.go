package binding_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/binding-service/internal/binding"
)

// batchItemJSON mirrors the wire shape of one batch result.
type batchItemJSON struct {
	Line    int          `json:"line"`
	Type    string       `json:"type"`
	Value   string       `json:"value"`
	Status  string       `json:"status"`
	Binding *bindingJSON `json:"binding"`
}

type batchResponseJSON struct {
	Results []batchItemJSON `json:"results"`
}

type fieldErrorJSON struct {
	Location string `json:"location"`
	Message  string `json:"message"`
}

type batchErrorJSON struct {
	Error struct {
		Code        string           `json:"code"`
		FieldErrors []fieldErrorJSON `json:"field_errors"`
	} `json:"error"`
}

const batchPath = "/api/v1/bindings/batch-lookup"

func readAll(resp *http.Response) ([]byte, error) {
	return io.ReadAll(resp.Body)
}

func newHTTPServer(pool *pgxpool.Pool) *httptest.Server {
	return httptest.NewServer(binding.NewRouter(binding.NewHandler(binding.NewStore(pool))))
}

func (e *testEnv) batch(t *testing.T, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(e.server.URL+batchPath, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func parseBatch(t *testing.T, raw []byte) batchResponseJSON {
	t.Helper()
	var r batchResponseJSON
	require.NoError(t, json.Unmarshal(raw, &r))
	return r
}

func parseBatchError(t *testing.T, raw []byte) batchErrorJSON {
	t.Helper()
	var r batchErrorJSON
	require.NoError(t, json.Unmarshal(raw, &r))
	return r
}

func batchItem(line int, typ, value string) string {
	return fmt.Sprintf(`{"line":%d,"type":%q,"value":%q}`, line, typ, value)
}

func batchBody(items ...string) string {
	return `{"queries":[` + strings.Join(items, ",") + `]}`
}

// batchRaw is HTTP-only: it never fails the test, so it is safe to call from
// the goroutines of the concurrent-snapshot test.
func (e *testEnv) batchRaw(body string) (int, []byte, error) {
	resp, err := http.Post(e.server.URL+batchPath, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

func resultByLine(res batchResponseJSON, line int) batchItemJSON {
	for _, r := range res.Results {
		if r.Line == line {
			return r
		}
	}
	return batchItemJSON{Line: -1}
}

// TestBatchLookupMixedFoundAndMissing covers acceptance path 1: a batch that
// mixes all three identifier kinds and includes missing rows must answer each
// item independently, preserve input order, echo the query coordinates and
// return the complete Binding structure on hits.
func TestBatchLookupMixedFoundAndMissing(t *testing.T) {
	env := newTestEnv(t)

	status, first := env.mustCreate(t, "REQ-B1", "CHIP-B1", "BOARD-B1")
	require.Equal(t, http.StatusCreated, status)
	want := parseBinding(t, first)

	status, raw := env.batch(t, batchBody(
		batchItem(7, "chip_uid", "CHIP-B1"),
		batchItem(2, "board_serial", "BOARD-MISSING"),
		batchItem(9, "request_key", "REQ-B1"),
		batchItem(1, "board_serial", "BOARD-B1"),
		batchItem(5, "chip_uid", "CHIP-MISSING"),
	))
	require.Equal(t, http.StatusOK, status, string(raw))

	res := parseBatch(t, raw)
	require.Len(t, res.Results, 5)

	// Order follows the input exactly, not the line numbers.
	expect := []struct {
		line   int
		typ    string
		value  string
		status string
	}{
		{7, "chip_uid", "CHIP-B1", "FOUND"},
		{2, "board_serial", "BOARD-MISSING", "NOT_FOUND"},
		{9, "request_key", "REQ-B1", "FOUND"},
		{1, "board_serial", "BOARD-B1", "FOUND"},
		{5, "chip_uid", "CHIP-MISSING", "NOT_FOUND"},
	}
	for i, ex := range expect {
		got := res.Results[i]
		assert.Equal(t, ex.line, got.Line, "item %d line echo", i)
		assert.Equal(t, ex.typ, got.Type, "item %d type echo", i)
		assert.Equal(t, ex.value, got.Value, "item %d value echo", i)
		assert.Equal(t, ex.status, got.Status, "item %d status", i)
		if ex.status == "FOUND" {
			require.NotNil(t, got.Binding, "item %d binding", i)
			assert.Equal(t, want, *got.Binding, "item %d returns the full existing Binding", i)
		} else {
			assert.Nil(t, got.Binding, "miss carries no binding, item %d", i)
		}
	}

	// A read-only batch never writes the request ledger.
	env.assertCounts(t, 1, 1)
}

// TestBatchLookupDuplicateQueriedOnce covers acceptance path 2: identical
// conditions are deduplicated for storage (one database query carrying the
// distinct keys) yet every input row still gets its own answer.
func TestBatchLookupDuplicateQueriedOnce(t *testing.T) {
	env := newTestEnv(t)

	status, first := env.mustCreate(t, "REQ-DUP", "CHIP-DUP", "BOARD-DUP")
	require.Equal(t, http.StatusCreated, status)
	want := parseBinding(t, first)

	// The same chip UID appears on three non-adjacent rows with different
	// line numbers; same for a missing request key.
	status, raw := env.batch(t, batchBody(
		batchItem(10, "chip_uid", "CHIP-DUP"),
		batchItem(20, "request_key", "REQ-MISSING"),
		batchItem(30, "chip_uid", "CHIP-DUP"),
		batchItem(40, "chip_uid", "CHIP-DUP"),
		batchItem(50, "request_key", "REQ-MISSING"),
	))
	require.Equal(t, http.StatusOK, status, string(raw))
	res := parseBatch(t, raw)
	require.Len(t, res.Results, 5)

	// Results stay in strict input order regardless of line numbering.
	assert.Equal(t, []int{10, 20, 30, 40, 50}, []int{
		res.Results[0].Line, res.Results[1].Line, res.Results[2].Line,
		res.Results[3].Line, res.Results[4].Line,
	})
	for _, line := range []int{10, 30, 40} {
		item := resultByLine(res, line)
		assert.Equal(t, "FOUND", item.Status, "line %d", line)
		require.NotNil(t, item.Binding, "line %d", line)
		assert.Equal(t, want, *item.Binding, "line %d carries the same single query result", line)
	}
	for _, line := range []int{20, 50} {
		item := resultByLine(res, line)
		assert.Equal(t, "NOT_FOUND", item.Status, "line %d", line)
		assert.Nil(t, item.Binding, "line %d", line)
	}

	// Storage-level proof: the whole batch is exactly one lookup SELECT
	// carrying only the two distinct (type, value) pairs.
	tracer, distinctValues := tracedBatch(t, testDSN(t), []binding.BatchQuery{
		{Line: 1, Type: binding.LookupByChipUID, Value: "CHIP-DUP"},
		{Line: 2, Type: binding.LookupByChipUID, Value: "CHIP-DUP"},
		{Line: 3, Type: binding.LookupByChipUID, Value: "CHIP-DUP"},
		{Line: 4, Type: binding.LookupByRequestKey, Value: "REQ-MISSING"},
		{Line: 5, Type: binding.LookupByRequestKey, Value: "REQ-MISSING"},
	})
	assert.Equal(t, 1, tracer.batchSelects, "batch must issue exactly one lookup SELECT, regardless of item count")
	assert.Equal(t, []string{"CHIP-DUP", "REQ-MISSING"}, distinctValues, "only distinct (type,value) pairs reach the database")
}

// TestBatchValidationRejected fixes the 422 contract for every invalid batch
// shape: empty array, over the limit, duplicate line numbers, unknown query
// type and illegal identifiers, each pinned to the offending field position.
func TestBatchValidationRejected(t *testing.T) {
	env := newTestEnv(t)
	env.mustCreate(t, "REQ-V", "CHIP-V", "BOARD-V")

	over := make([]string, 101)
	for i := range over {
		over[i] = batchItem(i+1, "chip_uid", fmt.Sprintf("CHIP-%03d", i+1))
	}

	cases := map[string]struct {
		body          string
		wantLocations []string
		wantContain   string
	}{
		"empty array": {
			body:          `{"queries":[]}`,
			wantLocations: []string{"queries"},
		},
		"missing queries key": {
			body:          `{}`,
			wantLocations: []string{"queries"},
		},
		"over the 100 limit": {
			body:          `{"queries":[` + strings.Join(over, ",") + `]}`,
			wantLocations: []string{"queries"},
			wantContain:   "100",
		},
		"duplicate line numbers": {
			body: batchBody(
				batchItem(3, "chip_uid", "CHIP-V"),
				batchItem(3, "board_serial", "BOARD-V"),
			),
			wantLocations: []string{"queries[1].line"},
			wantContain:   "duplicate line 3",
		},
		"zero line number": {
			body:          batchBody(batchItem(0, "chip_uid", "CHIP-V")),
			wantLocations: []string{"queries[0].line"},
		},
		"missing line": {
			body:          `{"queries":[{"type":"chip_uid","value":"CHIP-V"}]}`,
			wantLocations: []string{"queries[0].line"},
		},
		"null line": {
			body:          `{"queries":[{"line":null,"type":"chip_uid","value":"CHIP-V"}]}`,
			wantLocations: []string{"queries[0].line"},
		},
		"non-integer line": {
			body:          `{"queries":[{"line":4.5,"type":"chip_uid","value":"CHIP-V"}]}`,
			wantLocations: []string{"queries[0].line"},
		},
		"wrong type for line": {
			body:          `{"queries":[{"line":"7","type":"chip_uid","value":"CHIP-V"}]}`,
			wantLocations: []string{"queries[0].line"},
		},
		"unknown query type": {
			body:          batchBody(batchItem(1, "serial_number", "BOARD-V")),
			wantLocations: []string{"queries[0].type"},
		},
		"missing query type": {
			body:          `{"queries":[{"line":1,"value":"CHIP-V"}]}`,
			wantLocations: []string{"queries[0].type"},
		},
		"illegal identifier value": {
			body:          batchBody(batchItem(1, "chip_uid", "lowercase")),
			wantLocations: []string{"queries[0].value"},
		},
		"empty value": {
			body:          batchBody(batchItem(1, "chip_uid", "")),
			wantLocations: []string{"queries[0].value"},
		},
		"unknown field rejected": {
			body:          `{"queries":[{"line":1,"type":"chip_uid","value":"CHIP-V","extra":1}]}`,
			wantLocations: []string{"queries[0].extra"},
		},
		"malformed json": {
			body:          `{"queries":[`,
			wantLocations: []string{""},
		},
		"queries not an array": {
			body:          `{"queries":{}}`,
			wantLocations: []string{"queries"},
		},
		"null queries": {
			body:          `{"queries":null}`,
			wantLocations: []string{"queries"},
		},
		"null item": {
			body:          `{"queries":[null]}`,
			wantLocations: []string{"queries[0].line", "queries[0].type", "queries[0].value"},
		},
		"scalar item": {
			body:          `{"queries":[42]}`,
			wantLocations: []string{"queries[0]"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			status, raw := env.batch(t, tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
			errBody := parseBatchError(t, raw)
			assert.Equal(t, "VALIDATION_FAILED", errBody.Error.Code)
			require.Len(t, errBody.Error.FieldErrors, len(tc.wantLocations), string(raw))
			for i, loc := range tc.wantLocations {
				assert.Equal(t, loc, errBody.Error.FieldErrors[i].Location, string(raw))
			}
			if tc.wantContain != "" {
				assert.Contains(t, errBody.Error.FieldErrors[0].Message, tc.wantContain)
			}
		})
	}

	// Multiple bad items are all located in input order; the batch is
	// rejected wholesale and nothing is queried or stored.
	status, raw := env.batch(t, batchBody(
		batchItem(1, "chip_uid", "bad-id"),
		batchItem(2, "wat", "ALSO_BAD"),
	))
	require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
	errBody := parseBatchError(t, raw)
	require.Len(t, errBody.Error.FieldErrors, 3)
	assert.Equal(t, "queries[0].value", errBody.Error.FieldErrors[0].Location)
	assert.Equal(t, "queries[1].type", errBody.Error.FieldErrors[1].Location)
	assert.Equal(t, "queries[1].value", errBody.Error.FieldErrors[2].Location)
	env.assertCounts(t, 1, 1)
}

// TestBatchSingleItemBoundary checks the 1..100 bounds inclusive: exactly one
// and exactly one hundred items are accepted.
func TestBatchSingleItemBoundary(t *testing.T) {
	env := newTestEnv(t)
	env.mustCreate(t, "REQ-1", "CHIP-1", "BOARD-1")

	status, raw := env.batch(t, batchBody(batchItem(1, "request_key", "REQ-1")))
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Len(t, parseBatch(t, raw).Results, 1)

	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = batchItem(i+1, "chip_uid", fmt.Sprintf("CHIP-MISSING-%03d", i+1))
	}
	status, raw = env.batch(t, `{"queries":[`+strings.Join(hundred, ",")+`]}`)
	require.Equal(t, http.StatusOK, status, string(raw))
	res := parseBatch(t, raw)
	require.Len(t, res.Results, 100)
	for _, r := range res.Results {
		assert.Equal(t, "NOT_FOUND", r.Status)
	}
}

// TestBatchSnapshotUnderConcurrentWrites covers acceptance path 3: while
// bindings are being committed continuously, each batch must observe a single
// snapshot. The three identifiers of every target row are queried as separate
// (far apart) items; per row they must be all FOUND with the same record or
// all NOT_FOUND, never a torn mix.
func TestBatchSnapshotUnderConcurrentWrites(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// 32 rows x 3 identifiers = 96 items, staying inside the 100-item cap.
	const rows = 32

	// Writers commit new bindings continuously while batches run.
	var writers sync.WaitGroup
	writers.Add(rows)
	for i := 0; i < rows; i++ {
		go func(i int) {
			defer writers.Done()
			time.Sleep(time.Duration(i%7) * 3 * time.Millisecond)
			tx, err := env.pool.Begin(ctx)
			if err != nil {
				return
			}
			defer tx.Rollback(ctx)
			key := fmt.Sprintf("REQ-SNAP-%02d", i)
			if _, err := tx.Exec(ctx,
				`INSERT INTO requests (request_key, chip_uid, board_serial) VALUES ($1,$2,$3)`,
				key, fmt.Sprintf("CHIP-SNAP-%02d", i), fmt.Sprintf("BOARD-SNAP-%02d", i)); err != nil {
				return
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO bindings (request_key, chip_uid, board_serial) VALUES ($1,$2,$3)`,
				key, fmt.Sprintf("CHIP-SNAP-%02d", i), fmt.Sprintf("BOARD-SNAP-%02d", i)); err != nil {
				return
			}
			_ = tx.Commit(ctx)
		}(i)
	}

	// Batches interleave with the commits. Items are laid out in three blocks
	// so a row's three identifiers land far apart inside one batch.
	var mu sync.Mutex
	var batches []batchResponseJSON
	var readers sync.WaitGroup
	for round := 0; round < 16; round++ {
		readers.Add(1)
		go func(round int) {
			defer readers.Done()
			// Spread the batches across the whole ~20 ms commit window so
			// several snapshots are guaranteed to be taken while some, but not
			// all, rows are committed.
			time.Sleep(time.Duration(round) * time.Millisecond)
			items := make([]string, 0, 3*rows)
			for kind := 0; kind < 3; kind++ {
				for i := 0; i < rows; i++ {
					line := kind*rows + i + 1
					switch kind {
					case 0:
						items = append(items, batchItem(line, "request_key", fmt.Sprintf("REQ-SNAP-%02d", i)))
					case 1:
						items = append(items, batchItem(line, "chip_uid", fmt.Sprintf("CHIP-SNAP-%02d", i)))
					default:
						items = append(items, batchItem(line, "board_serial", fmt.Sprintf("BOARD-SNAP-%02d", i)))
					}
				}
			}
			status, raw, err := env.batchRaw(`{"queries":[` + strings.Join(items, ",") + `]}`)
			if err != nil || status != http.StatusOK {
				return
			}
			var parsed batchResponseJSON
			if json.Unmarshal(raw, &parsed) != nil {
				return
			}
			mu.Lock()
			batches = append(batches, parsed)
			mu.Unlock()
		}(round)
	}
	readers.Wait()
	writers.Wait()
	require.NotEmpty(t, batches, "at least one batch should complete during write churn")

	sawPartial := false
	for bi, res := range batches {
		require.Len(t, res.Results, 3*rows)
		byLine := make(map[int]batchItemJSON, 3*rows)
		for _, item := range res.Results {
			byLine[item.Line] = item
		}
		foundRows := 0
		for i := 0; i < rows; i++ {
			byKey := byLine[1+i]
			byChip := byLine[rows+1+i]
			byBoard := byLine[2*rows+1+i]

			// Input order and echo preserved.
			assert.Equal(t, "request_key", byKey.Type, "batch %d row %d", bi, i)
			assert.Equal(t, fmt.Sprintf("REQ-SNAP-%02d", i), byKey.Value, "batch %d row %d", bi, i)

			found := 0
			for _, s := range []string{byKey.Status, byChip.Status, byBoard.Status} {
				if s == "FOUND" {
					found++
				}
			}
			// Single snapshot: either the row was visible to the whole batch
			// (all three FOUND, identical record) or to none of it.
			switch found {
			case 3:
				foundRows++
				require.NotNil(t, byKey.Binding, "batch %d row %d key", bi, i)
				require.NotNil(t, byChip.Binding, "batch %d row %d chip", bi, i)
				require.NotNil(t, byBoard.Binding, "batch %d row %d board", bi, i)
				assert.Equal(t, *byKey.Binding, *byChip.Binding, "batch %d row %d snapshot torn across identifiers", bi, i)
				assert.Equal(t, *byKey.Binding, *byBoard.Binding, "batch %d row %d snapshot torn across identifiers", bi, i)
			case 0:
				assert.Nil(t, byKey.Binding, "batch %d row %d", bi, i)
				assert.Nil(t, byChip.Binding, "batch %d row %d", bi, i)
				assert.Nil(t, byBoard.Binding, "batch %d row %d", bi, i)
			default:
				t.Fatalf("batch %d row %d has mixed FOUND/NOT_FOUND across its three identifiers: snapshot torn", bi, i)
			}
		}
		if foundRows > 0 && foundRows < rows {
			sawPartial = true
		}
	}
	assert.True(t, sawPartial, "at least one batch should observe commits mid-flight (partial visibility) to prove real overlap")

	// After writers settle, a final batch finds every row all three ways.
	items := make([]string, 0, 3*rows)
	for i := 0; i < rows; i++ {
		items = append(items,
			batchItem(3*i+1, "request_key", fmt.Sprintf("REQ-SNAP-%02d", i)),
			batchItem(3*i+2, "chip_uid", fmt.Sprintf("CHIP-SNAP-%02d", i)),
			batchItem(3*i+3, "board_serial", fmt.Sprintf("BOARD-SNAP-%02d", i)),
		)
	}
	status, raw := env.batch(t, `{"queries":[`+strings.Join(items, ",")+`]}`)
	require.Equal(t, http.StatusOK, status, string(raw))
	for _, r := range parseBatch(t, raw).Results {
		assert.Equal(t, "FOUND", r.Status, "line %d", r.Line)
	}
}

// TestBatchDatabaseFailureReturns500 pins the error contract: a database
// failure during the read is a 500, never a per-item 422 or 404.
func TestBatchDatabaseFailureReturns500(t *testing.T) {
	_ = testDSN(t) // skip when no integration database is configured
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	require.NoError(t, err)
	deadPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	deadPool.Close()

	srv := newHTTPServer(deadPool)
	defer srv.Close()

	resp, err := http.Post(srv.URL+batchPath, "application/json",
		strings.NewReader(batchBody(batchItem(1, "chip_uid", "CHIP-1"))))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, string(raw))
	assert.Equal(t, "INTERNAL", parseError(t, raw).Error.Code)
}

// queryCounter is a pgx QueryTracer counting the lookup SELECTs a pool issues.
type queryCounter struct {
	mu              sync.Mutex
	batchSelects    int
	batchSelectArgs []any
}

func (q *queryCounter) TraceQueryStart(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	q.mu.Lock()
	defer q.mu.Unlock()
	if strings.Contains(data.SQL, "FROM unnest(") {
		q.batchSelects++
		q.batchSelectArgs = data.Args
	}
	return context.Background()
}

func (q *queryCounter) TraceQueryEnd(_ context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {}

// tracedBatch runs one BatchLookup on a dedicated traced pool and reports how
// many lookup SELECTs it issued and which identifier values reached the SQL.
func tracedBatch(t *testing.T, dsn string, queries []binding.BatchQuery) (*queryCounter, []string) {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	tracer := &queryCounter{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()

	items, err := binding.NewStore(pool).BatchLookup(ctx, queries)
	require.NoError(t, err)
	require.Len(t, items, len(queries))

	// The unnest SELECT receives three arrays: lines, kinds and values.
	require.Len(t, tracer.batchSelectArgs, 3)
	values, ok := tracer.batchSelectArgs[2].([]string)
	require.True(t, ok, "values argument should be []string, got %T", tracer.batchSelectArgs[2])
	return tracer, values
}

// TestBatchKeepsExistingEndpointsCompatible is a cheap guard that the create
// endpoint and the three single-item GETs still respond exactly as before
// alongside the new route (acceptance path 4, complementing the existing
// create/replay/query test coverage).
func TestBatchKeepsExistingEndpointsCompatible(t *testing.T) {
	env := newTestEnv(t)

	status, created := env.mustCreate(t, "REQ-COMPAT", "CHIP-COMPAT", "BOARD-COMPAT")
	require.Equal(t, http.StatusCreated, status)

	// Replay still returns the original record with 200.
	status, replay := env.mustCreate(t, "REQ-COMPAT", "CHIP-COMPAT", "BOARD-COMPAT")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, parseBinding(t, created), parseBinding(t, replay))

	for _, path := range []string{
		"/api/v1/bindings/by-request-key/REQ-COMPAT",
		"/api/v1/bindings/by-chip-uid/CHIP-COMPAT",
		"/api/v1/bindings/by-board-serial/BOARD-COMPAT",
	} {
		status, raw := env.get(t, path)
		require.Equal(t, http.StatusOK, status, path)
		assert.Equal(t, parseBinding(t, created), parseBinding(t, raw), path)
	}
	status, raw := env.batch(t, batchBody(batchItem(1, "request_key", "REQ-COMPAT")))
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Equal(t, "FOUND", parseBatch(t, raw).Results[0].Status)
}
