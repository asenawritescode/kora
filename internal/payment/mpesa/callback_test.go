package mpesa

import (
	"errors"
	"testing"
)

func TestParseCallback(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		id, receipt string
		desc        string
		success     bool
		wantErr     bool
		missingID   bool
	}{
		{
			name: "success",
			body: `{"Body":{"stkCallback":{"MerchantRequestID":"m1","CheckoutRequestID":"ws_CO_1","ResultCode":0,"ResultDesc":"The service request is processed successfully.","CallbackMetadata":{"Item":[{"Name":"Amount","Value":1},{"Name":"MpesaReceiptNumber","Value":"NLJ7RT61SV"}]}}}}`,
			id:   "ws_CO_1", success: true, receipt: "NLJ7RT61SV", desc: "The service request is processed successfully.",
		},
		{
			name: "cancelled 1032",
			body: `{"Body":{"stkCallback":{"CheckoutRequestID":"ws_CO_2","ResultCode":1032,"ResultDesc":"Request cancelled by user"}}}`,
			id:   "ws_CO_2", desc: "Request cancelled by user",
		},
		{
			name:    "missing checkout id",
			body:    `{"Body":{"stkCallback":{"ResultCode":0}}}`,
			wantErr: true, missingID: true,
		},
		{name: "malformed json", body: `{"Body":`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok, receipt, desc, err := ParseCallback([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.missingID && !errors.Is(err, ErrMissingCheckoutRequestID) {
				t.Fatalf("err = %v, want missing id", err)
			}
			if id != tt.id || ok != tt.success || receipt != tt.receipt || desc != tt.desc {
				t.Fatalf("got %q %v %q %q", id, ok, receipt, desc)
			}
		})
	}
}

func TestTokenValid(t *testing.T) {
	tests := []struct {
		provided, expected string
		want               bool
	}{
		{"abc", "abc", true}, {"abd", "abc", false}, {"", "", false}, {"", "abc", false},
	}
	for _, tt := range tests {
		if got := TokenValid(tt.provided, tt.expected); got != tt.want {
			t.Errorf("TokenValid(%q,%q)=%v", tt.provided, tt.expected, got)
		}
	}
}
