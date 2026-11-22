package ai

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestPendingMoveAllowsOnlyTheDocumentedTransitions walks every (from, to) pair of the states.
func TestPendingMoveAllowsOnlyTheDocumentedTransitions(t *testing.T) {
	states := []string{pendingPreviewed, pendingExecuting, pendingDone, pendingFailed}
	allowed := map[[2]string]bool{
		{pendingPreviewed, pendingExecuting}: true,
		{pendingExecuting, pendingExecuting}: true,
		{pendingExecuting, pendingDone}:      true,
		{pendingExecuting, pendingFailed}:    true,
	}
	for _, from := range states {
		for _, to := range states {
			err := pendingMove(from, to)
			want := allowed[[2]string{from, to}]
			if (err == nil) != want {
				t.Errorf("pendingMove(%s, %s) = %v, want allowed=%v", from, to, err, want)
			}
		}
	}
}

// TestWriteIDIsRandomAndPrefixed checks that each preview gets its own pw_ id.
func TestWriteIDIsRandomAndPrefixed(t *testing.T) {
	first, second := newWriteID(), newWriteID()
	if first == second || !strings.HasPrefix(first, "pw_") || len(first) != len("pw_")+26 {
		t.Fatalf("write ids %q and %q must differ and be pw_ plus a 26-character ULID", first, second)
	}
}

func TestPendingArgsHashChangesWithArguments(t *testing.T) {
	base := `{"command":"record.create","doctype":"Item","fields":{"price":250}}`
	changed := `{"command":"record.create","doctype":"Item","fields":{"price":260}}`
	if pendingArgsHash(base) == pendingArgsHash(changed) {
		t.Fatal("a change to the stored arguments must change the hash")
	}
}

// TestAIToolErrorShape checks the wire form every structured tool error takes.
func TestAIToolErrorShape(t *testing.T) {
	tests := []struct {
		name string
		err  aiToolError
		want []string
	}{
		{name: "code and message", err: aiToolError{Code: errNotFound, Message: "Gone."}, want: []string{`{"error":{"code":"not_found","message":"Gone."}}`}},
		{name: "field and fix", err: aiToolError{Code: errInvalidValue, Message: "Bad.", Field: "price", Fix: "Use a number."}, want: []string{`"field":"price"`, `"fix":"Use a number."`}},
		{name: "valid values", err: aiToolError{Code: errInvalidValue, Message: "Bad.", ValidValues: []string{"a", "b"}}, want: []string{`"valid_values":["a","b"]`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.text()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("text() = %s, want it to contain %s", got, want)
				}
			}
			if !isToolError(got) {
				t.Fatalf("structured error %s must be reported as a tool error", got)
			}
		})
	}
}

// TestRecordWriteErrorMapping maps kernel errors to the structured codes.
func TestRecordWriteErrorMapping(t *testing.T) {
	required := func(fields ...string) []byte {
		var issues doctype.ValidationErrors
		for _, f := range fields {
			issues = append(issues, &doctype.ValidationError{Type: "ValidationError", Message: f + " is required.", Field: strings.ToLower(strings.ReplaceAll(f, " ", "_")), DocType: "Item"})
		}
		raw, err := json.Marshal(issues)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{name: "all missing fields in one error", err: &contract.Error{Type: contract.CodeValidationFailed, Message: "validation failed", Details: required("Item Name", "Price")}, want: []string{`"code":"missing_fields"`, "Item Name, Price"}},
		{name: "permission", err: &contract.Error{Type: contract.CodePermissionDenied, Message: "no"}, want: []string{`"code":"permission_denied"`}},
		{name: "not found", err: &contract.Error{Type: contract.CodeNotFound, Message: "missing"}, want: []string{`"code":"not_found"`}},
		{name: "validation without detail is a guard", err: &contract.Error{Type: contract.CodeValidationFailed, Message: "stock short"}, want: []string{`"code":"guard_failed"`, "stock short"}},
		{name: "plain error keeps a safe code", err: errors.New("boom"), want: []string{`"code":"invalid_value"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := recordWriteErrorText(tc.err)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("text = %s, want it to contain %s", got, want)
				}
			}
		})
	}
}

// TestPendingSummaryNamesTheWrite checks the one-line description each open preview gives the next turn.
func TestPendingSummaryNamesTheWrite(t *testing.T) {
	tests := []struct {
		name string
		row  pendingWrite
		want string
	}{
		{name: "create", row: pendingWrite{ToolName: toolCreateRecord, PreviewJSON: `{"action":"create","record_type":"Item","fields":{"item_name":"Tea","price":50,"notes":null}}`}, want: `create Item (item_name Tea, price 50)`},
		{name: "update shows old and new", row: pendingWrite{ToolName: toolUpdateRecord, PreviewJSON: `{"action":"update","record_type":"Item","name":"ITM-1","changes":{"price":{"old":180,"new":200}}}`}, want: `update Item ITM-1 (price 180 -> 200)`},
		{name: "pack action uses its title", row: pendingWrite{ToolName: toolRunAction, PreviewJSON: `{"action":"pos.core~pos.drawer~open_session","title":"Open till session","input":{"register":"REG-1","opening_float":500}}`}, want: `Open till session (opening_float 500, register REG-1)`},
		{name: "unreadable preview", row: pendingWrite{PreviewJSON: `not json`}, want: `a previewed write`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.summary(); got != tc.want {
				t.Fatalf("summary() = %q, want %q", got, tc.want)
			}
		})
	}
}
