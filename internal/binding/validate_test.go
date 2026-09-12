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
