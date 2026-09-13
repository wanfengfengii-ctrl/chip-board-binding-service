package binding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// errMultipleJSONValues marks a body carrying more than one top-level JSON
// value; every entry point accepts exactly one.
var errMultipleJSONValues = errors.New("json: body must contain exactly one JSON value")

// duplicateKeyError marks a JSON object that names the same member twice.
// encoding/json would silently let the last occurrence win (RFC 8259 leaves
// duplicate member behavior undefined); every entry point treats a repeated
// field as an ambiguous request and rejects the whole body before any of the
// conflicting values is used.
type duplicateKeyError struct {
	Key string
}

func (e *duplicateKeyError) Error() string {
	return fmt.Sprintf("json: duplicate object member %q", e.Key)
}

// validateUniqueJSON scans exactly one JSON value and rejects any object that
// repeats a member name, an unclosed value, or any trailing data after the
// single value. It streams tokens instead of building a DOM so the 1 MiB batch
// body cap stays cheap.
//
// When rootOnly is true only members of the root object are checked; objects
// nested deeper are still parsed (the whole body must be well-formed JSON) but
// their keys are left to a per-item validation pass that can pinpoint the
// array position.
func validateUniqueJSON(body []byte, rootOnly bool) error {
	dec := json.NewDecoder(bytes.NewReader(body))

	type frame struct {
		object    bool
		expectKey bool
		keys      map[string]struct{}
	}
	var stack []frame
	consumed := 0 // number of completed top-level values (must end at exactly one)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) > 0 {
				return io.ErrUnexpectedEOF
			}
			if consumed == 0 {
				return io.EOF // empty body: the same error a plain decode would return
			}
			return nil
		}
		if err != nil {
			return err
		}
		// Token streaming allows back-to-back JSON values; the entry points do not.
		if consumed > 0 && len(stack) == 0 {
			return errMultipleJSONValues
		}

		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				stack = append(stack, frame{object: t == '{', expectKey: true, keys: map[string]struct{}{}})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					consumed++
					break
				}
				// The closed child satisfied the enclosing object's member value.
				if stack[len(stack)-1].object {
					stack[len(stack)-1].expectKey = true
				}
			}
		case string:
			if len(stack) == 0 {
				consumed++ // scalar root value
				break
			}
			f := &stack[len(stack)-1]
			if f.object && f.expectKey {
				if !rootOnly || len(stack) == 1 {
					if _, exists := f.keys[t]; exists {
						return &duplicateKeyError{Key: t}
					}
					f.keys[t] = struct{}{}
				}
				f.expectKey = false
			} else if f.object {
				f.expectKey = true // string member value
			}
		default: // number, bool, nil
			if len(stack) == 0 {
				consumed++
				break
			}
			if stack[len(stack)-1].object {
				stack[len(stack)-1].expectKey = true
			}
		}
	}
}

// decodeUniqueObject reads a body that must be exactly one JSON value, rejects
// repeated member names and trailing data, then unmarshals into target with
// unknown fields rejected. A scalar or array root passes the shape scan but
// fails the typed decode with encoding/json's usual UnmarshalTypeError, so
// existing malformed-body handling is unchanged.
func decodeUniqueObject(r io.Reader, target any) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return decodeUniqueJSON(body, target, false)
}

// decodeUniqueJSON first validates member-name uniqueness of body (rootOnly
// limits the check to the root object) and then unmarshals it into target with
// unknown fields rejected.
func decodeUniqueJSON(body []byte, target any, rootOnly bool) error {
	if err := validateUniqueJSON(body, rootOnly); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}
