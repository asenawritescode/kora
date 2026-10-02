package api

import (
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
