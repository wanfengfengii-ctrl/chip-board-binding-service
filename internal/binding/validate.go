package binding

import "regexp"

// identPattern enforces 1-64 characters of ASCII uppercase letters, digits or
// hyphens. \A and \z anchor to the absolute start and end of the string.
var identPattern = regexp.MustCompile(`\A[A-Z0-9-]{1,64}\z`)

const identRule = "must be 1-64 characters of A-Z, 0-9 or '-'"

// ValidationError reports every field that failed validation.
type ValidationError struct {
	Fields map[string]string
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
