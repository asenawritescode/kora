package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/gin-gonic/gin"
)

func TestWriteKernelErrorPreservesHTTPErrorSemantics(t *testing.T) {
	tests := []struct {
		name string
		code contract.Code
		want int
	}{
		{name: "permission denied", code: contract.CodePermissionDenied, want: http.StatusForbidden},
		{name: "unauthenticated", code: contract.CodeUnauthenticated, want: http.StatusUnauthorized},
		{name: "invalid payload", code: contract.CodeValidationFailed, want: http.StatusBadRequest},
		{name: "missing record", code: contract.CodeNotFound, want: http.StatusNotFound},
		{name: "stale version", code: contract.CodeConflict, want: http.StatusConflict},
		{name: "idempotency key reused", code: contract.CodeIdempotencyKeyReused, want: http.StatusConflict},
		{name: "deadline", code: contract.CodeDeadlineExceeded, want: http.StatusGatewayTimeout},
	}

	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			handler.writeKernelError(ctx, contract.NewError(test.code, test.name))
			if recorder.Code != test.want {
				t.Fatalf("HTTP status = %d, want %d: %s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

func TestWriteKernelErrorPreservesValidationFieldDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	validationDetails := json.RawMessage(`[{"type":"ValidationError","message":"Cashier is required.","field":"cashier","doctype":"Shift"}]`)
	cerr := contract.NewError(contract.CodeValidationFailed, "validation failed")
	cerr.Details = validationDetails

	handler.writeKernelError(ctx, cerr)

	var response struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	fields, ok := response.Error.Details["fields"].([]any)
	if !ok || len(fields) != 1 {
		t.Fatalf("validation fields = %#v, want one preserved field error", response.Error.Details["fields"])
	}
	field, ok := fields[0].(map[string]any)
	if !ok || field["field"] != "cashier" || field["message"] != "Cashier is required." {
		t.Fatalf("preserved validation field = %#v", fields[0])
	}
}
