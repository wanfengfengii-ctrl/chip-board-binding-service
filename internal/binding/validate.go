package binding

import (
	"fmt"
	"regexp"
)

// identPattern enforces 1-64 characters of ASCII uppercase letters, digits or
// hyphens. \A and \z anchor to the absolute start and end of the string.
var identPattern = regexp.MustCompile(`\A[A-Z0-9-]{1,64}\z`)

const identRule = "must be 1-64 characters of A-Z, 0-9 or '-'"

// ValidationError reports every field that failed validation.
type ValidationError struct {
	// Fields names the offending fields of a single-resource request.
	Fields map[string]string
	// FieldErrors pinpoints each offending field of a batch request,
	// including its position in the input array.
	FieldErrors []FieldError
}

// FieldError locates one invalid field inside a request. Location is a
// JSON-pointer-ish path such as "queries[3].value".
type FieldError struct {
	Location string `json:"location"`
	Message  string `json:"message"`
}

func (e *ValidationError) Error() string { return "validation failed" }

// ValidateCreateRequest checks all three identifiers. The whole request is
// rejected (and nothing is stored) when any single field is invalid.
func ValidateCreateRequest(req CreateRequest) *ValidationError {
	fields := make(map[string]string)
	if !identPattern.MatchString(req.RequestKey) {
		fields["request_key"] = identRule
	}
	if !identPattern.MatchString(req.ChipUID) {
		fields["chip_uid"] = identRule
	}
	if !identPattern.MatchString(req.BoardSerial) {
		fields["board_serial"] = identRule
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

const (
	batchRequiredRule = "line is required and must be a positive integer"
	batchTypeRule     = "type must be one of chip_uid, board_serial or request_key"
	batchValueRule    = identRule
)

// ValidateBatchQueries checks the shape of a batch lookup request: one to
// MaxBatchQueries items, unique positive line numbers, a known type and a
// legal identifier on every item. The whole batch is rejected on any failure;
// nothing is queried or stored. Returned FieldError values pinpoint the
// offending field positions in input order.
func ValidateBatchQueries(req BatchQueryRequest) (queries []BatchQuery, verr *ValidationError) {
	var errs []FieldError
	add := func(location, message string) {
		errs = append(errs, FieldError{Location: location, Message: message})
	}

	if len(req.Queries) == 0 {
		add("queries", "queries must contain between 1 and 100 items")
		return nil, &ValidationError{FieldErrors: errs}
	}
	if len(req.Queries) > MaxBatchQueries {
		add("queries", fmt.Sprintf("queries must contain at most %d items, got %d", MaxBatchQueries, len(req.Queries)))
		return nil, &ValidationError{FieldErrors: errs}
	}

	queries = make([]BatchQuery, len(req.Queries))
	seenLines := make(map[int]int, len(req.Queries))
	for i, item := range req.Queries {
		lineOK := item.Line != nil && *item.Line > 0
		if item.Line == nil || *item.Line <= 0 {
			add(fmt.Sprintf("queries[%d].line", i), batchRequiredRule)
		} else if first, dup := seenLines[*item.Line]; dup {
			add(fmt.Sprintf("queries[%d].line", i), fmt.Sprintf("duplicate line %d, first used at queries[%d].line", *item.Line, first))
		} else {
			seenLines[*item.Line] = i
		}

		var lookupType LookupType
		switch LookupType(item.Type) {
		case LookupByChipUID, LookupByBoardSerial, LookupByRequestKey:
			lookupType = LookupType(item.Type)
		default:
			add(fmt.Sprintf("queries[%d].type", i), batchTypeRule)
		}

		if !identPattern.MatchString(item.Value) {
			add(fmt.Sprintf("queries[%d].value", i), batchValueRule)
		}

		if lineOK && lookupType != "" && identPattern.MatchString(item.Value) {
			queries[i] = BatchQuery{Line: *item.Line, Type: lookupType, Value: item.Value}
		}
	}

	if len(errs) > 0 {
		return nil, &ValidationError{FieldErrors: errs}
	}
	return queries, nil
}
