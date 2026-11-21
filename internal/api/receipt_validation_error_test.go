package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
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

func TestWriteReceiptValidationErrorCarriesFieldDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw, err := json.Marshal(doctype.ValidationErrors{{Type: "required", Message: "Sale date is required.", Field: "sale_date", DocType: "Sale"}})
	if err != nil {
		t.Fatal(err)
	}
	refusal := contract.NewError(contract.CodeValidationFailed, "validation failed")
	refusal.Details = raw
	tests := []struct {
		name    string
		receipt contract.OperationReceipt
		want    []string
	}{
		{"generic validation refusal names the field", contract.OperationReceipt{Status: contract.ReceiptRejected, Error: refusal}, []string{`"fields"`, `"field":"sale_date"`, `"message":"Sale date is required."`, `"doctype":"Sale"`}},
		{"schema refusal keeps its field", contract.OperationReceipt{Status: contract.ReceiptRejected, Error: contract.NewError(contract.CodeValidationFailed, "at '/till_session': missing")}, []string{`"field":"till_session"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			if !writeReceiptValidationError(c, tc.receipt) {
				t.Fatal("refusal not handled")
			}
			for _, want := range tc.want {
				if !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("body %s lacks %s", rec.Body.String(), want)
				}
			}
		})
	}
}
