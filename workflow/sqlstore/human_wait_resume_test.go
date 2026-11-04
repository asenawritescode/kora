package sqlstore

import (
	"database/sql"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestHumanWaitOutcomeRoutesResolvedAndTerminalRequests(t *testing.T) {
	spec := &contract.HumanSpec{
		Outcomes: map[contract.ResponseKind]contract.StepID{
			contract.ResponseApprove: "approved", contract.ResponseReject: "rejected",
			contract.ResponseReturn: "returned", contract.ResponseProvide: "provided",
			contract.ResponseAcknowledge: "acknowledged", contract.ResponseComplete: "completed",
		},
		OnExpired: "expired", OnCancelled: "cancelled", OnStale: "stale",
	}
	for _, test := range []struct {
		name, status, outcome string
		want                  contract.StepID
	}{
		{"approved", "resolved", "approved", "approved"},
		{"rejected", "resolved", "rejected", "rejected"},
		{"returned", "resolved", "returned_for_changes", "returned"},
		{"provided", "resolved", "information_provided", "provided"},
		{"acknowledged", "resolved", "acknowledged", "acknowledged"},
		{"completed", "resolved", "completed", "completed"},
		{"expired", "expired", "", "expired"},
		{"cancelled", "cancelled", "", "cancelled"},
		{"stale", "stale", "", "stale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := humanWaitOutcome(spec, test.status, sql.NullString{String: test.outcome, Valid: test.outcome != ""})
			if err != nil || got != test.want {
				t.Fatalf("humanWaitOutcome() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestHumanWaitOutcomeRefusesUnmappedOrNonterminalRequests(t *testing.T) {
	spec := &contract.HumanSpec{Outcomes: map[contract.ResponseKind]contract.StepID{}, OnExpired: "expired", OnCancelled: "cancelled", OnStale: "stale"}
	for _, test := range []struct {
		status  string
		outcome sql.NullString
	}{
		{status: "resolved", outcome: sql.NullString{String: "approved", Valid: true}},
		{status: "resolved", outcome: sql.NullString{}},
		{status: "pending", outcome: sql.NullString{}},
	} {
		if _, err := humanWaitOutcome(spec, test.status, test.outcome); err == nil {
			t.Fatalf("humanWaitOutcome(%q,%+v) accepted missing/invalid route", test.status, test.outcome)
		}
	}
}
