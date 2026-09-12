package binding

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

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
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Field   string            `json:"field,omitempty"`
	Details map[string]string `json:"details,omitempty"`
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
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:    CodeValidationFailed,
			Message: "body must be a JSON object with request_key, chip_uid and board_serial string fields",
		})
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusUnprocessableEntity, apiError{
			Code:    CodeValidationFailed,
			Message: "body must contain exactly one JSON object",
		})
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
	h.get(c, func() (Binding, error) {
		return h.store.GetByRequestKey(c.Request.Context(), c.Param("request_key"))
	})
}

// GetByChipUID handles GET /api/v1/bindings/by-chip-uid/:chip_uid.
func (h *Handler) GetByChipUID(c *gin.Context) {
	h.get(c, func() (Binding, error) {
		return h.store.GetByChipUID(c.Request.Context(), c.Param("chip_uid"))
	})
}

// GetByBoardSerial handles GET /api/v1/bindings/by-board-serial/:board_serial.
func (h *Handler) GetByBoardSerial(c *gin.Context) {
	h.get(c, func() (Binding, error) {
		return h.store.GetByBoardSerial(c.Request.Context(), c.Param("board_serial"))
	})
}

func (h *Handler) get(c *gin.Context, fetch func() (Binding, error)) {
	b, err := fetch()
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

// Health handles GET /healthz.
func (h *Handler) Health(c *gin.Context) {
	if err := h.store.Ping(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func writeError(c *gin.Context, status int, e apiError) {
	c.JSON(status, errorEnvelope{Error: e})
}
