package api

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestMPesaCallbackMatchesTerminalOnlyForSameResult(t *testing.T) {
	newOperation := func(status, receipt, message string) *doctype.Document {
		doc := doctype.NewDocument("External Operation")
		doc.Set("status", status)
		doc.Set("provider_reference", receipt)
		doc.Set("error_message", message)
		return doc
	}
	withReceipt := mpesaSTKCallback{ResultCode: 0}
	withReceipt.CallbackMetadata.Item = []struct {
		Name  string `json:"Name"`
		Value any    `json:"Value"`
	}{{Name: "MpesaReceiptNumber", Value: "RCP-1"}}
	withoutReceipt := mpesaSTKCallback{ResultCode: 0}
	failed := mpesaSTKCallback{ResultCode: 1032, ResultDesc: "cancelled by user"}
	cases := []struct {
		name      string
		operation *doctype.Document
		callback  mpesaSTKCallback
		want      bool
	}{
		{name: "same success and receipt", operation: newOperation("Succeeded", "RCP-1", ""), callback: withReceipt, want: true},
		{name: "success without provider receipt", operation: newOperation("Succeeded", "", ""), callback: withoutReceipt, want: true},
		{name: "different receipt is not replay", operation: newOperation("Succeeded", "RCP-2", ""), callback: withReceipt},
		{name: "failed result and message", operation: newOperation("Failed", "", "cancelled by user"), callback: failed, want: true},
		{name: "different result is not replay", operation: newOperation("Succeeded", "RCP-1", ""), callback: failed},
		{name: "different failure message is not replay", operation: newOperation("Failed", "", "insufficient funds"), callback: failed},
		{name: "nil operation is not replay", callback: withReceipt},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := mpesaCallbackMatchesTerminal(test.operation, test.callback); got != test.want {
				t.Fatalf("match = %v, want %v", got, test.want)
			}
		})
	}
}
