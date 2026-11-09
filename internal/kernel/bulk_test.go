package kernel

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestRunBulkPartialFailureAndKeys(t *testing.T) {
	rows := []BulkRow{{1, json.RawMessage(`1`)}, {2, json.RawMessage(`2`)}, {3, json.RawMessage(`3`)}}
	var keys []string
	exec := func(op Operation) (contract.CommandResult, *contract.Error) {
		keys = append(keys, op.Context.IdempotencyKey)
		if string(op.Payload) == "2" {
			return contract.CommandResult{}, contract.NewError(contract.CodeValidationFailed, "bad row")
		}
		return contract.CommandResult{Status: contract.StatusCompleted}, nil
	}
	out := runBulk(Operation{Command: "x"}, rows, BulkOptions{ImportID: "imp"}, exec)
	if len(out) != 3 || out[0].Error != nil || out[1].Error == nil || out[2].Error != nil {
		t.Fatalf("partial failure not reported per row: %+v", out)
	}
	if keys[0] != BulkRowKey("imp", 1) || keys[0] == keys[1] {
		t.Fatalf("keys not per-row: %v", keys)
	}
	if BulkRowKey("imp", 1) != BulkRowKey("imp", 1) || BulkRowKey("imp", 1) == BulkRowKey("other", 1) {
		t.Fatal("key not deterministic/scoped")
	}
}
