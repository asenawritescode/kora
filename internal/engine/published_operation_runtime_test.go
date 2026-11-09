package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
)

func TestKernelCommandProviderPayloadWrapsPublishedInput(t *testing.T) {
	got := kernelCommandPayload([]byte(`{"title":"invoice"}`))
	var decoded struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("wrapped payload is invalid JSON: %v", err)
	}
	if decoded.Data["title"] != "invoice" {
		t.Fatalf("wrapped payload data = %#v", decoded.Data)
	}
}

func TestPublishedOperationEntryPointsRejectIncompleteInput(t *testing.T) {
	tests := []struct {
		name string
		call func() error
	}{
		{"execute without runtime", func() error {
			_, err := executePublishedOperation(context.Background(), publishedOperationRuntime{}, publishedOperationTarget{}, publishedOperationInvocation{})
			return err
		}},
		{"lookup without runtime", func() error {
			_, _, err := lookupPublishedOperation(context.Background(), publishedOperationRuntime{}, publishedOperationTarget{}, "key")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typed := asContractError(tt.call())
			if typed == nil || typed.Type != contract.CodeValidationFailed {
				t.Fatalf("error = %v, want %s", typed, contract.CodeValidationFailed)
			}
		})
	}
}

func TestOperationReceiptsFailRejectsInvalidReceiptBeforeWriting(t *testing.T) {
	_, err := operationReceipts{}.fail(context.Background(), configstore.PublishedOperationAttempt{}, contract.ReceiptRejected, nil)
	if typed := asContractError(err); typed == nil || typed.Type != contract.CodeInternal {
		t.Fatalf("fail error = %v, want %s", err, contract.CodeInternal)
	}
}
