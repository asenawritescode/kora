package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/gin-gonic/gin"
)

func TestWriteReceiptValidationError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		receipt contract.OperationReceipt
		handled bool
		field   string
	}{
		{"unknown property", contract.OperationReceipt{Status: contract.ReceiptRejected, Error: contract.NewError(contract.CodeValidationFailed, "at '': additional properties 'currency' not allowed")}, true, `"field":"currency"`},
		{"missing property path", contract.OperationReceipt{Status: contract.ReceiptRejected, Error: contract.NewError(contract.CodeValidationFailed, "at '/till_session': missing")}, true, `"field":"till_session"`},
		{"failed receipt untouched", contract.OperationReceipt{Status: contract.ReceiptFailed, Error: contract.NewError(contract.CodeInternal, "x")}, false, ""},
		{"completed untouched", contract.OperationReceipt{Status: contract.ReceiptCompleted}, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			if got := writeReceiptValidationError(c, tc.receipt); got != tc.handled {
				t.Fatalf("handled = %v", got)
			}
			if tc.handled && (rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.field)) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
