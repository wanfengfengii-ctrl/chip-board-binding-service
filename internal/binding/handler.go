package binding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Error codes returned in the error envelope. REQUEST_KEY_CONFLICT and
// DEVICE_ALREADY_BOUND let a failing station tell "request key reused with a
// different payload" apart from "device already bound".
const (
	CodeValidationFailed   = "VALIDATION_FAILED"
	CodeRequestKeyConflict = "REQUEST_KEY_CONFLICT"
	CodeDeviceAlreadyBound = "DEVICE_ALREADY_BOUND"
	CodeNotFound           = "NOT_FOUND"
	CodeInternal           = "INTERNAL"
)

type errorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	Field       string            `json:"field,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
	FieldErrors []FieldError      `json:"field_errors,omitempty"`
}

// Handler exposes the binding service over HTTP.
type Handler struct {
	store *Store
}

// NewHandler returns a Handler backed by store.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// Create handles POST /api/v1/bindings.
func (h *Handler) Create(c *gin.Context) {
	var req CreateRequest
	if err := decodeUniqueObject(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &req); err != nil {
		h.writeCreateDecodeError(c, err)
		return
	}
	if verr := ValidateCreateRequest(req); verr != nil {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:    CodeValidationFailed,
			Message: "identifiers must be 1-64 characters of A-Z, 0-9 or '-'",
			Details: verr.Fields,
		})
		return
	}

	res, err := h.store.Create(c.Request.Context(), req)
	if err != nil {
		writeError(c, http.StatusInternalServerError, apiError{Code: CodeInternal, Message: "internal error"})
		return
	}
	switch res.Outcome {
	case OutcomeCreated:
		c.JSON(http.StatusCreated, res.Binding)
	case OutcomeReplayed:
		// Identical replay of an earlier request: return the original
		// result exactly as stored.
		c.JSON(http.StatusOK, res.Binding)
	case OutcomeRequestKeyConflict:
		c.JSON(http.StatusConflict, gin.H{
			"error": apiError{
				Code:    CodeRequestKeyConflict,
				Message: "request key was already used with a different payload",
			},
			"existing": res.Binding,
		})
	case OutcomeDeviceOccupied:
		c.JSON(http.StatusConflict, gin.H{
			"error": apiError{
				Code:    CodeDeviceAlreadyBound,
				Message: "identifier is already bound to another record",
				Field:   res.Field,
			},
		})
	}
}

// GetByRequestKey handles GET /api/v1/bindings/by-request-key/:request_key.
func (h *Handler) GetByRequestKey(c *gin.Context) {
	h.get(c, "request_key", h.store.GetByRequestKey)
}

// GetByChipUID handles GET /api/v1/bindings/by-chip-uid/:chip_uid.
func (h *Handler) GetByChipUID(c *gin.Context) {
	h.get(c, "chip_uid", h.store.GetByChipUID)
}

// GetByBoardSerial handles GET /api/v1/bindings/by-board-serial/:board_serial.
func (h *Handler) GetByBoardSerial(c *gin.Context) {
	h.get(c, "board_serial", h.store.GetByBoardSerial)
}

// get resolves one single-item lookup. The path identifier is validated up
// front: an illegal identifier rejects the whole request with 422 (nothing is
// queried), exactly like an illegal field on create. A legal identifier that
// matches no record is the ordinary 404.
func (h *Handler) get(c *gin.Context, param string, fetch func(context.Context, string) (Binding, error)) {
	id := c.Param(param)
	if verr := ValidateLookupIdentifier(param, id); verr != nil {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:    CodeValidationFailed,
			Message: "identifiers must be 1-64 characters of A-Z, 0-9 or '-'",
			Details: verr.Fields,
		})
		return
	}
	b, err := fetch(c.Request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(c, http.StatusNotFound, apiError{Code: CodeNotFound, Message: "binding not found"})
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, apiError{Code: CodeInternal, Message: "internal error"})
		return
	}
	c.JSON(http.StatusOK, b)
}

// batchResponse is the payload of a successful batch lookup: results stay in
// the exact order of the input items.
type batchResponse struct {
	Results []BatchItem `json:"results"`
}

// BatchLookup handles POST /api/v1/bindings/batch-lookup. A repair technician
// submits one to one hundred numbered queries, each by chip UID, board serial
// or request key. The whole batch is validated up front (422 on any shape
// error); otherwise every item is resolved and answered independently.
func (h *Handler) BatchLookup(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err != nil {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:        CodeValidationFailed,
			Message:     "body must be a JSON object smaller than 1 MiB",
			FieldErrors: []FieldError{{Location: "", Message: "could not read request body"}},
		})
		return
	}

	// First decode only the envelope, rejecting trailing data, a repeated
	// top-level key (a second "queries" would otherwise silently replace the
	// first array), unknown top-level fields or a non-array queries value.
	var envelope struct {
		Queries []json.RawMessage `json:"queries"`
	}
	if err := decodeUniqueJSON(body, &envelope, true); err != nil {
		// A wrong-typed queries value pinpoints "queries"; a repeated queries
		// key pinpoints it too; every other malformed-envelope error concerns
		// the whole body.
		location := ""
		message := "body must be a JSON object with a queries array of {line,type,value} items"
		var typeErr *json.UnmarshalTypeError
		var dupErr *duplicateKeyError
		switch {
		case errors.As(err, &typeErr) && typeErr.Field != "":
			location = strings.ToLower(typeErr.Field)
			message = fmt.Sprintf("%s has the wrong JSON type", location)
		case errors.As(err, &dupErr):
			location = dupErr.Key
			message = fmt.Sprintf("%s appears more than once; the request structure must be unambiguous", dupErr.Key)
		case errors.Is(err, errMultipleJSONValues):
			message = "body must contain exactly one JSON object"
		}
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:        CodeValidationFailed,
			Message:     message,
			FieldErrors: []FieldError{{Location: location, Message: message}},
		})
		return
	}

	// Decode each item on its own so a wrong field type, an unknown field or a
	// repeated member name can be pinned to its exact array position.
	req := BatchQueryRequest{Queries: make([]BatchQueryItem, len(envelope.Queries))}
	var fieldErrs []FieldError
	for i, raw := range envelope.Queries {
		var item BatchQueryItem
		err := decodeUniqueJSON(raw, &item, false)
		if err != nil {
			location := fmt.Sprintf("queries[%d]", i)
			message := err.Error()
			var typeErr *json.UnmarshalTypeError
			var dupErr *duplicateKeyError
			switch {
			case errors.As(err, &typeErr):
				if typeErr.Field != "" {
					// The decoder reports the bare struct field ("line");
					// prefix it with the item position.
					field := strings.ToLower(typeErr.Field)
					location = fmt.Sprintf("queries[%d].%s", i, field)
					message = fmt.Sprintf("%s has the wrong JSON type (got %s)", field, typeErr.Value)
				} else {
					// The item itself is not a JSON object.
					location = fmt.Sprintf("queries[%d]", i)
					message = "query item must be a JSON object"
				}
			case errors.As(err, &dupErr):
				// A repeated line/type/value is ambiguous: encoding/json would
				// keep the last occurrence, so the whole batch is rejected.
				location = fmt.Sprintf("queries[%d].%s", i, dupErr.Key)
				message = fmt.Sprintf("%s appears more than once; the query must be unambiguous", dupErr.Key)
			case errors.Is(err, errMultipleJSONValues):
				location = fmt.Sprintf("queries[%d]", i)
				message = "each query must be exactly one JSON object"
			}
			if typeErr == nil && dupErr == nil && !errors.Is(err, errMultipleJSONValues) {
				if name, ok := unknownFieldName(err); ok {
					location = fmt.Sprintf("queries[%d].%s", i, name)
				}
			}
			fieldErrs = append(fieldErrs, FieldError{Location: location, Message: message})
			continue
		}
		req.Queries[i] = item
	}

	if len(fieldErrs) > 0 {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:        CodeValidationFailed,
			Message:     "batch queries failed validation",
			FieldErrors: fieldErrs,
		})
		return
	}

	queries, verr := ValidateBatchQueries(req)
	if verr != nil {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:        CodeValidationFailed,
			Message:     "batch queries failed validation",
			FieldErrors: verr.FieldErrors,
		})
		return
	}

	items, err := h.store.BatchLookup(c.Request.Context(), queries)
	if err != nil {
		writeError(c, http.StatusInternalServerError, apiError{Code: CodeInternal, Message: "internal error"})
		return
	}
	c.JSON(http.StatusOK, batchResponse{Results: items})
}

// CreateInspection handles POST /api/v1/inspections. A repair technician
// scans a chip UID and a board serial together before teardown; both scanned
// values are resolved against the existing bindings in one transaction and
// the verdict (CONSISTENT, MISMATCH, PARTIAL or UNREGISTERED) is stored as an
// immutable inspection record. Illegal identifiers reject the whole request
// with 422 and nothing is stored.
func (h *Handler) CreateInspection(c *gin.Context) {
	var req InspectionRequest
	if err := decodeUniqueObject(http.MaxBytesReader(c.Writer, c.Request.Body, 4096), &req); err != nil {
		h.writeInspectionDecodeError(c, err)
		return
	}
	if verr := ValidateInspectionRequest(req); verr != nil {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:    CodeValidationFailed,
			Message: "identifiers must be 1-64 characters of A-Z, 0-9 or '-'",
			Details: verr.Fields,
		})
		return
	}

	insp, err := h.store.CreateInspection(c.Request.Context(), req)
	if err != nil {
		writeError(c, http.StatusInternalServerError, apiError{Code: CodeInternal, Message: "internal error"})
		return
	}
	c.JSON(http.StatusCreated, insp)
}

// GetInspection handles GET /api/v1/inspections/:inspection_id. A repair
// technician reviews the original, immutable verdict of a past scan. A path
// segment that is not a positive integer is a 422 validation error (nothing
// is queried); a legal id without a record is a 404.
func (h *Handler) GetInspection(c *gin.Context) {
	raw := c.Param("inspection_id")
	// ParseInt keeps its range check, but it accepts a leading sign; a path id
	// must be a canonical positive integer ("1", not "+1", "-1" or "01").
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || raw[0] < '1' || raw[0] > '9' {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:    CodeValidationFailed,
			Message: "inspection_id must be a positive integer",
			Details: map[string]string{"inspection_id": "must be a positive integer"},
		})
		return
	}

	insp, err := h.store.GetInspection(c.Request.Context(), id)
	if errors.Is(err, ErrInspectionNotFound) {
		writeError(c, http.StatusNotFound, apiError{Code: CodeNotFound, Message: "inspection not found"})
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, apiError{Code: CodeInternal, Message: "internal error"})
		return
	}
	c.JSON(http.StatusOK, insp)
}

// GetMismatchPeers handles GET /api/v1/bindings/:binding_id/mismatch-peers. A
// repair supervisor reviewing a suspected mix-up sees every other binding the
// target was implicated with in mismatch inspections, most repeated first, so
// the worst relations can be investigated without paging through inspection
// records one by one. A path id that is not a positive integer or a limit
// outside 1-50 is a 422 (nothing is queried); a legal id without a binding is
// a 404; a binding with no mismatch history yields an empty peers array.
func (h *Handler) GetMismatchPeers(c *gin.Context) {
	raw := c.Param("binding_id")
	// Same canonical positive-integer rule as the inspection review path.
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || raw[0] < '1' || raw[0] > '9' {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:    CodeValidationFailed,
			Message: "binding_id must be a positive integer",
			Details: map[string]string{"binding_id": "must be a positive integer"},
		})
		return
	}

	limit := DefaultMismatchPeerLimit
	if rawLimit, ok := c.GetQuery("limit"); ok {
		n, perr := strconv.ParseInt(rawLimit, 10, 64)
		if perr != nil || n < 1 || n > MaxMismatchPeerLimit || rawLimit[0] < '1' || rawLimit[0] > '9' {
			writeError(c, http.StatusUnprocessableEntity, apiError{
				Code:    CodeValidationFailed,
				Message: "limit must be an integer between 1 and 50",
				Details: map[string]string{"limit": "must be an integer between 1 and 50"},
			})
			return
		}
		limit = int(n)
	}

	res, err := h.store.GetMismatchPeers(c.Request.Context(), id, limit)
	if errors.Is(err, ErrNotFound) {
		writeError(c, http.StatusNotFound, apiError{Code: CodeNotFound, Message: "binding not found"})
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, apiError{Code: CodeInternal, Message: "internal error"})
		return
	}
	c.JSON(http.StatusOK, res)
}

// unknownFieldName extracts the field name from a json "unknown field" error.
func unknownFieldName(err error) (string, bool) {
	msg := err.Error()
	const prefix = `json: unknown field "`
	if !strings.HasPrefix(msg, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(msg, prefix)
	name, ok := strings.CutSuffix(name, `"`)
	return name, ok
}

// Health handles GET /healthz.
func (h *Handler) Health(c *gin.Context) {
	if err := h.store.Ping(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// writeCreateDecodeError maps a request-body decode failure of the binding
// create endpoint to its 422 envelope. A repeated member name pinpoints the
// ambiguous field; every other malformed-body error keeps the generic message.
func (h *Handler) writeCreateDecodeError(c *gin.Context, err error) {
	message := "body must be a JSON object with request_key, chip_uid and board_serial string fields"
	var details map[string]string
	var dupErr *duplicateKeyError
	switch {
	case errors.As(err, &dupErr):
		message = fmt.Sprintf("field %s appears more than once; every field must carry a single unambiguous value", dupErr.Key)
		details = map[string]string{dupErr.Key: "must appear at most once"}
	case errors.Is(err, errMultipleJSONValues):
		message = "body must contain exactly one JSON object"
	}
	writeError(c, http.StatusUnprocessableEntity, apiError{
		Code:    CodeValidationFailed,
		Message: message,
		Details: details,
	})
}

// writeInspectionDecodeError is the inspection-create counterpart of
// writeCreateDecodeError: a repeated scan field makes the verdict ambiguous, so
// the whole request is rejected before any value is used.
func (h *Handler) writeInspectionDecodeError(c *gin.Context, err error) {
	message := "body must be a JSON object with chip_uid and board_serial string fields"
	var details map[string]string
	var dupErr *duplicateKeyError
	switch {
	case errors.As(err, &dupErr):
		message = fmt.Sprintf("field %s appears more than once; every scanned field must carry a single unambiguous value", dupErr.Key)
		details = map[string]string{dupErr.Key: "must appear at most once"}
	case errors.Is(err, errMultipleJSONValues):
		message = "body must contain exactly one JSON object"
	}
	writeError(c, http.StatusUnprocessableEntity, apiError{
		Code:    CodeValidationFailed,
		Message: message,
		Details: details,
	})
}

func writeError(c *gin.Context, status int, e apiError) {
	c.JSON(status, errorEnvelope{Error: e})
}
