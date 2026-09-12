package binding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreateRequest(t *testing.T) {
	valid := func() CreateRequest {
		return CreateRequest{RequestKey: "REQ-1", ChipUID: "CHIP-9", BoardSerial: "BOARD-7"}
	}

	t.Run("valid request passes", func(t *testing.T) {
		assert.Nil(t, ValidateCreateRequest(valid()))
	})

	t.Run("boundary lengths pass", func(t *testing.T) {
		req := CreateRequest{
			RequestKey:  "K",
			ChipUID:     strings.Repeat("C", 64),
			BoardSerial: strings.Repeat("B", 64),
		}
		assert.Nil(t, ValidateCreateRequest(req))
	})

	cases := map[string]struct {
		mutate func(*CreateRequest)
		field  string
	}{
		"empty request key":      {func(r *CreateRequest) { r.RequestKey = "" }, "request_key"},
		"lowercase request key":  {func(r *CreateRequest) { r.RequestKey = "req-1" }, "request_key"},
		"overlong request key":   {func(r *CreateRequest) { r.RequestKey = strings.Repeat("K", 65) }, "request_key"},
		"space in chip uid":      {func(r *CreateRequest) { r.ChipUID = "CHIP 1" }, "chip_uid"},
		"underscore in chip uid": {func(r *CreateRequest) { r.ChipUID = "CHIP_1" }, "chip_uid"},
		"slash in chip uid":      {func(r *CreateRequest) { r.ChipUID = "CHIP/1" }, "chip_uid"},
		"non-ascii chip uid":     {func(r *CreateRequest) { r.ChipUID = "CHIP-é" }, "chip_uid"},
		"newline in chip uid":    {func(r *CreateRequest) { r.ChipUID = "CHIP\n1" }, "chip_uid"},
		"empty board serial":     {func(r *CreateRequest) { r.BoardSerial = "" }, "board_serial"},
		"overlong board serial":  {func(r *CreateRequest) { r.BoardSerial = strings.Repeat("B", 65) }, "board_serial"},
		"lowercase board serial": {func(r *CreateRequest) { r.BoardSerial = "board-7" }, "board_serial"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req := valid()
			tc.mutate(&req)
			verr := ValidateCreateRequest(req)
			require.NotNil(t, verr)
			assert.Contains(t, verr.Fields, tc.field)
		})
	}

	t.Run("all fields invalid at once", func(t *testing.T) {
		verr := ValidateCreateRequest(CreateRequest{})
		require.NotNil(t, verr)
		assert.Len(t, verr.Fields, 3)
	})
}

func TestValidateLookupIdentifier(t *testing.T) {
	t.Run("legal identifiers pass", func(t *testing.T) {
		assert.Nil(t, ValidateLookupIdentifier("chip_uid", "CHIP-9"))
		assert.Nil(t, ValidateLookupIdentifier("request_key", "K"))
		assert.Nil(t, ValidateLookupIdentifier("board_serial", strings.Repeat("B", 64)))
	})

	cases := map[string]string{
		"empty":       "",
		"lowercase":   "chip-9",
		"underscore":  "CHIP_9",
		"space":       "CHIP 9",
		"slash":       "CHIP/9",
		"non-ascii":   "CHIP-é",
		"overlong":    strings.Repeat("C", 65),
		"punctuation": "CHIP.9",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			verr := ValidateLookupIdentifier("chip_uid", value)
			require.NotNil(t, verr)
			assert.Equal(t, map[string]string{"chip_uid": identRule}, verr.Fields)
		})
	}

	t.Run("names the offending identifier", func(t *testing.T) {
		verr := ValidateLookupIdentifier("request_key", "req-1")
		require.NotNil(t, verr)
		assert.Contains(t, verr.Fields, "request_key")
	})
}

func intptr(n int) *int { return &n }

func batchItemOn(line *int, typ, value string) BatchQueryItem {
	return BatchQueryItem{Line: line, Type: typ, Value: value}
}

func TestValidateBatchQueries(t *testing.T) {
	t.Run("valid mixed batch normalizes in order", func(t *testing.T) {
		req := BatchQueryRequest{Queries: []BatchQueryItem{
			batchItemOn(intptr(3), "chip_uid", "CHIP-1"),
			batchItemOn(intptr(1), "board_serial", "BOARD-2"),
			batchItemOn(intptr(2), "request_key", "REQ-3"),
		}}
		queries, verr := ValidateBatchQueries(req)
		require.Nil(t, verr)
		require.Len(t, queries, 3)
		assert.Equal(t, BatchQuery{Line: 3, Type: LookupByChipUID, Value: "CHIP-1"}, queries[0])
		assert.Equal(t, BatchQuery{Line: 1, Type: LookupByBoardSerial, Value: "BOARD-2"}, queries[1])
		assert.Equal(t, BatchQuery{Line: 2, Type: LookupByRequestKey, Value: "REQ-3"}, queries[2])
	})

	t.Run("boundaries one and one hundred pass", func(t *testing.T) {
		one := BatchQueryRequest{Queries: []BatchQueryItem{batchItemOn(intptr(1), "chip_uid", "C")}}
		_, verr := ValidateBatchQueries(one)
		assert.Nil(t, verr)

		hundred := make([]BatchQueryItem, 100)
		for i := range hundred {
			hundred[i] = batchItemOn(intptr(i+1), "chip_uid", "C")
		}
		_, verr = ValidateBatchQueries(BatchQueryRequest{Queries: hundred})
		assert.Nil(t, verr)
	})

	cases := map[string]struct {
		items         []BatchQueryItem
		wantLocations []string
	}{
		"empty array": {
			items:         nil,
			wantLocations: []string{"queries"},
		},
		"over the limit": {
			items:         repeatItems(101),
			wantLocations: []string{"queries"},
		},
		"duplicate line numbers": {
			items: []BatchQueryItem{
				batchItemOn(intptr(5), "chip_uid", "CHIP-1"),
				batchItemOn(intptr(5), "board_serial", "BOARD-1"),
			},
			wantLocations: []string{"queries[1].line"},
		},
		"missing, zero and negative line": {
			items: []BatchQueryItem{
				batchItemOn(nil, "chip_uid", "CHIP-1"),
				batchItemOn(intptr(0), "chip_uid", "CHIP-1"),
				batchItemOn(intptr(-9), "chip_uid", "CHIP-1"),
			},
			wantLocations: []string{"queries[0].line", "queries[1].line", "queries[2].line"},
		},
		"unknown query type": {
			items:         []BatchQueryItem{batchItemOn(intptr(1), "uid", "CHIP-1")},
			wantLocations: []string{"queries[0].type"},
		},
		"missing query type": {
			items:         []BatchQueryItem{batchItemOn(intptr(1), "", "CHIP-1")},
			wantLocations: []string{"queries[0].type"},
		},
		"illegal identifiers": {
			items: []BatchQueryItem{
				batchItemOn(intptr(1), "chip_uid", "lowercase"),
				batchItemOn(intptr(2), "board_serial", ""),
				batchItemOn(intptr(3), "request_key", strings.Repeat("K", 65)),
			},
			wantLocations: []string{"queries[0].value", "queries[1].value", "queries[2].value"},
		},
		"all problems located in input order": {
			items: []BatchQueryItem{
				batchItemOn(intptr(1), "chip_uid", "bad"),
				batchItemOn(intptr(2), "nope", strings.Repeat("B", 65)),
			},
			wantLocations: []string{"queries[0].value", "queries[1].type", "queries[1].value"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			queries, verr := ValidateBatchQueries(BatchQueryRequest{Queries: tc.items})
			require.NotNil(t, verr)
			assert.Nil(t, queries)
			require.Len(t, verr.FieldErrors, len(tc.wantLocations))
			for i, loc := range tc.wantLocations {
				assert.Equal(t, loc, verr.FieldErrors[i].Location)
			}
		})
	}
}

func repeatItems(n int) []BatchQueryItem {
	items := make([]BatchQueryItem, n)
	for i := range items {
		items[i] = batchItemOn(intptr(i+1), "chip_uid", "CHIP-1")
	}
	return items
}

func TestValidateInspectionRequest(t *testing.T) {
	t.Run("valid request passes", func(t *testing.T) {
		assert.Nil(t, ValidateInspectionRequest(InspectionRequest{ChipUID: "CHIP-9", BoardSerial: "BOARD-7"}))
	})

	t.Run("boundary lengths pass", func(t *testing.T) {
		assert.Nil(t, ValidateInspectionRequest(InspectionRequest{
			ChipUID:     strings.Repeat("C", 64),
			BoardSerial: "B",
		}))
	})

	cases := map[string]struct {
		req   InspectionRequest
		field string
	}{
		"empty chip uid":         {InspectionRequest{ChipUID: "", BoardSerial: "BOARD-7"}, "chip_uid"},
		"lowercase chip uid":     {InspectionRequest{ChipUID: "chip-9", BoardSerial: "BOARD-7"}, "chip_uid"},
		"underscore chip uid":    {InspectionRequest{ChipUID: "CHIP_9", BoardSerial: "BOARD-7"}, "chip_uid"},
		"overlong chip uid":      {InspectionRequest{ChipUID: strings.Repeat("C", 65), BoardSerial: "BOARD-7"}, "chip_uid"},
		"empty board serial":     {InspectionRequest{ChipUID: "CHIP-9", BoardSerial: ""}, "board_serial"},
		"lowercase board serial": {InspectionRequest{ChipUID: "CHIP-9", BoardSerial: "board-7"}, "board_serial"},
		"non-ascii board serial": {InspectionRequest{ChipUID: "CHIP-9", BoardSerial: "BOARD-é"}, "board_serial"},
		"overlong board serial":  {InspectionRequest{ChipUID: "CHIP-9", BoardSerial: strings.Repeat("B", 65)}, "board_serial"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			verr := ValidateInspectionRequest(tc.req)
			require.NotNil(t, verr)
			assert.Contains(t, verr.Fields, tc.field)
		})
	}

	t.Run("both fields invalid at once", func(t *testing.T) {
		verr := ValidateInspectionRequest(InspectionRequest{})
		require.NotNil(t, verr)
		assert.Len(t, verr.Fields, 2)
	})
}

func TestVerdict(t *testing.T) {
	id := func(n int64) *int64 { return &n }
	cases := []struct {
		name  string
		chip  *int64
		board *int64
		want  InspectionResult
	}{
		{"both sides same binding", id(1), id(1), ResultConsistent},
		{"sides hit different bindings", id(1), id(2), ResultMismatch},
		{"only chip side hit", id(1), nil, ResultPartial},
		{"only board side hit", nil, id(2), ResultPartial},
		{"neither side hit", nil, nil, ResultUnregistered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, verdict(tc.chip, tc.board))
		})
	}
}
