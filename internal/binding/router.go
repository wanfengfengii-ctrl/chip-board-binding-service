package binding

import "github.com/gin-gonic/gin"

// NewRouter builds the HTTP engine with all routes registered.
func NewRouter(h *Handler) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/healthz", h.Health)

	v1 := r.Group("/api/v1")
	v1.POST("/bindings", h.Create)
	v1.POST("/bindings/batch-lookup", h.BatchLookup)
	v1.GET("/bindings/by-request-key/:request_key", h.GetByRequestKey)
	v1.GET("/bindings/by-chip-uid/:chip_uid", h.GetByChipUID)
	v1.GET("/bindings/by-board-serial/:board_serial", h.GetByBoardSerial)
	return r
}
