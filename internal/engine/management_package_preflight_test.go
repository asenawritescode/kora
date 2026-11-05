package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/plugin"
)

func TestBindingPreflightFailedDetail(t *testing.T) {
	blockers := func(n int) []plugin.BindingDiagnostic {
		out := make([]plugin.BindingDiagnostic, n)
		for i := range out {
			out[i] = plugin.BindingDiagnostic{Key: plugin.BindingKey{ConsumerModuleID: contract.ModuleID(fmt.Sprintf("m%d", i)), CapabilityName: "cap.x"}, Code: "provider_selection_required"}
		}
		return out
	}
	tests := []struct {
		name         string
		err          error
		blockers     []plugin.BindingDiagnostic
		wantMessage  []string
		wantBlockers int
		maxCause     int
	}{
		{name: "error cause", err: errors.New(`module "inventory.core" has no matching owner command`), wantMessage: []string{`preflight: module "inventory.core" has no matching owner command`}},
		{name: "bounded cause", err: errors.New(strings.Repeat("é", 600)), maxCause: maxPreflightCauseBytes + len("...")},
		{name: "bounded blockers", blockers: blockers(12), wantBlockers: maxPreflightBlockers, wantMessage: []string{"m0/cap.x: provider_selection_required", "and 2 more"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := bindingPreflightFailed(tc.err, tc.blockers)
			if result.State != contract.OperationFailed || result.Error == nil || result.Error.Type != contract.CodeConflict {
				t.Fatalf("result = %+v", result)
			}
			for _, want := range tc.wantMessage {
				if !strings.Contains(result.Error.Message, want) {
					t.Fatalf("message %q does not contain %q", result.Error.Message, want)
				}
			}
			var details struct {
				Cause        string            `json:"cause"`
				Blockers     []json.RawMessage `json:"blockers"`
				BlockerCount int               `json:"blocker_count"`
			}
			if err := json.Unmarshal(result.Error.Details, &details); err != nil {
				t.Fatal(err)
			}
			if len(details.Blockers) != tc.wantBlockers || details.BlockerCount != len(tc.blockers) {
				t.Fatalf("details = %+v", details)
			}
			if tc.maxCause > 0 && (len(details.Cause) > tc.maxCause || !strings.HasSuffix(details.Cause, "...") || !json.Valid([]byte(`"`+details.Cause+`"`))) {
				t.Fatalf("cause not bounded: %d bytes", len(details.Cause))
			}
		})
	}
}
