package binding

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateUniqueJSON pins the scan contract shared by every write/parse
// entry point: the body must be exactly one well-formed JSON value, and no
// object may name the same member twice. encoding/json alone would silently
// keep the last occurrence; the service must reject such ambiguous requests.
func TestValidateUniqueJSON(t *testing.T) {
	t.Run("well-formed single values pass", func(t *testing.T) {
		for _, body := range []string{
			`{}`,
			`{"a":1}`,
			`{"a":1,"b":[1,1],"c":{"x":true,"y":null}}`,
			`[]`,
			`[1,1,{}]`,
			`"scalar"`,
			`42`,
			`null`,
		} {
			assert.NoError(t, validateUniqueJSON([]byte(body), false), body)
		}
	})

	t.Run("empty body is an EOF like a plain decode", func(t *testing.T) {
		assert.ErrorIs(t, validateUniqueJSON(nil, false), io.EOF)
		assert.ErrorIs(t, validateUniqueJSON([]byte("   \n"), false), io.EOF)
	})

	t.Run("duplicate root member rejected", func(t *testing.T) {
		var dup *duplicateKeyError
		err := validateUniqueJSON([]byte(`{"a":1,"a":2}`), false)
		require.ErrorAs(t, err, &dup)
		assert.Equal(t, "a", dup.Key)
	})

	t.Run("duplicate member with identical value is still rejected", func(t *testing.T) {
		var dup *duplicateKeyError
		err := validateUniqueJSON([]byte(`{"queries":[],"queries":[]}`), false)
		require.ErrorAs(t, err, &dup)
		assert.Equal(t, "queries", dup.Key)
	})

	t.Run("duplicate nested member rejected in full scan", func(t *testing.T) {
		var dup *duplicateKeyError
		err := validateUniqueJSON([]byte(`{"a":{"x":1,"x":2}}`), false)
		require.ErrorAs(t, err, &dup)
		assert.Equal(t, "x", dup.Key)
	})

	t.Run("duplicate member inside an array element rejected in full scan", func(t *testing.T) {
		var dup *duplicateKeyError
		err := validateUniqueJSON([]byte(`{"queries":[{"line":1,"line":2}]}`), false)
		require.ErrorAs(t, err, &dup)
		assert.Equal(t, "line", dup.Key)
	})

	t.Run("root-only scan ignores nested objects", func(t *testing.T) {
		assert.NoError(t, validateUniqueJSON([]byte(`{"queries":[{"type":"a","type":"b"}]}`), true))
		var dup *duplicateKeyError
		err := validateUniqueJSON([]byte(`{"queries":[],"queries":[]}`), true)
		require.ErrorAs(t, err, &dup)
		assert.Equal(t, "queries", dup.Key)
	})

	t.Run("escaped member names are normalized before comparing", func(t *testing.T) {
		var dup *duplicateKeyError
		err := validateUniqueJSON([]byte(`{"chip_uid":"A","chip_uid":"B"}`), false)
		require.ErrorAs(t, err, &dup)
		assert.Equal(t, "chip_uid", dup.Key)
	})

	t.Run("repeated scalar array elements are not duplicates", func(t *testing.T) {
		assert.NoError(t, validateUniqueJSON([]byte(`{"v":[1,1,1]}`), false))
	})

	t.Run("multiple top-level values rejected", func(t *testing.T) {
		assert.ErrorIs(t, validateUniqueJSON([]byte(`{"a":1}{}`), false), errMultipleJSONValues)
		assert.ErrorIs(t, validateUniqueJSON([]byte(`{"a":1} {}`), false), errMultipleJSONValues)
		assert.ErrorIs(t, validateUniqueJSON([]byte(`1 2`), false), errMultipleJSONValues)
	})

	t.Run("truncated body rejected", func(t *testing.T) {
		assert.ErrorIs(t, validateUniqueJSON([]byte(`{"a":`), false), io.ErrUnexpectedEOF)
		assert.ErrorIs(t, validateUniqueJSON([]byte(`{"a":[1,2`), false), io.ErrUnexpectedEOF)
	})

	t.Run("syntax error rejected", func(t *testing.T) {
		assert.Error(t, validateUniqueJSON([]byte(`{,}`), false))
		assert.Error(t, validateUniqueJSON([]byte(`{"a":1,}`), false))
	})
}

// TestDecodeUniqueObject covers the convenience wrapper: duplicate keys,
// trailing values and unknown fields all surface as decode errors.
func TestDecodeUniqueObject(t *testing.T) {
	type obj struct {
		A string `json:"a"`
		B int    `json:"b"`
	}

	var o obj
	require.NoError(t, decodeUniqueObject(bytes.NewReader([]byte(`{"a":"x","b":2}`)), &o))
	assert.Equal(t, obj{A: "x", B: 2}, o)

	var dup *duplicateKeyError
	require.ErrorAs(t, decodeUniqueObject(bytes.NewReader([]byte(`{"a":"x","a":"y"}`)), &obj{}), &dup)

	assert.ErrorIs(t, decodeUniqueObject(bytes.NewReader([]byte(`{"a":"x"}{}`)), &obj{}), errMultipleJSONValues)

	err := decodeUniqueObject(bytes.NewReader([]byte(`{"a":"x","nope":1}`)), &obj{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")

	// A duplicate key is rejected before the typed decode even when the last
	// value alone would have been perfectly valid.
	err = decodeUniqueObject(bytes.NewReader([]byte(`{"a":"x","b":1,"b":2}`)), &obj{})
	require.ErrorAs(t, err, &dup)
	assert.Equal(t, "b", dup.Key)
}

// TestValidateUniqueJSONNestedKeyOrdering ensures the key/value bookkeeping
// survives alternating arrays, nested objects and scalar members.
func TestValidateUniqueJSONNestedStructure(t *testing.T) {
	body := `{"a":[1,{"k":1,"j":2},"x"],"b":{"k":[1,2,3],"j":{"q":null}},"c":true}`
	require.NoError(t, validateUniqueJSON([]byte(body), false))

	var dup *duplicateKeyError
	err := validateUniqueJSON([]byte(`{"a":[{"k":1}],"b":{"k":2},"b":3}`), false)
	require.ErrorAs(t, err, &dup)
	assert.Equal(t, "b", dup.Key)
}
