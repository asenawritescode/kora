package recon

import (
	"testing"
	"time"
)

func TestCompareBusinessObligationsFindingsAreStableAndScoped(t *testing.T) {
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	input := BusinessReconciliationInput{Cutoff: cutoff, GracePeriod: time.Hour, MaxRows: 20, SourceReady: true, TargetReady: true,
		Sources: []BusinessObligation{
			{Key: "invoice:1", SourceRef: "sale:1", EntityID: "entity-a", Amount: "12.50", Currency: "KES", OccurredAt: cutoff.Add(-2 * time.Hour)},
			{Key: "invoice:2", SourceRef: "sale:2", EntityID: "entity-a", Amount: "4.00", Currency: "KES", OccurredAt: cutoff.Add(-10 * time.Minute)},
			{Key: "invoice:3", SourceRef: "sale:3", EntityID: "entity-a", Amount: "8.00", Currency: "KES", OccurredAt: cutoff.Add(-2 * time.Hour)},
		}, Targets: []BusinessEffect{
			{Key: "invoice:3", TargetRef: "invoice:b", EntityID: "entity-b", Amount: "8.00", Currency: "KES"},
			{Key: "invoice:3", TargetRef: "invoice:c", EntityID: "entity-a", Amount: "8.00", Currency: "KES"},
		}}
	first, err := CompareBusinessObligations(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompareBusinessObligations(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Findings) != 3 || len(second.Findings) != len(first.Findings) {
		t.Fatalf("findings = %+v", first.Findings)
	}
	byKind := map[BusinessFindingKind]BusinessFinding{}
	for _, finding := range first.Findings {
		byKind[finding.Kind] = finding
	}
	if byKind[FindingMissingEffect].Key != "invoice:1" || byKind[FindingPendingGrace].Key != "invoice:2" || byKind[FindingDuplicateEffect].Key != "invoice:3" {
		t.Fatalf("unexpected finding kinds: %+v", first.Findings)
	}
	for i := range first.Findings {
		if first.Findings[i].ID != second.Findings[i].ID {
			t.Fatal("finding identities changed across retry")
		}
	}
}

func TestCompareBusinessObligationsMarksUnavailableIncompleteAndEnforcesBounds(t *testing.T) {
	base := BusinessReconciliationInput{Cutoff: time.Now().UTC(), MaxRows: 1, SourceReady: true, TargetReady: false}
	result, err := CompareBusinessObligations(base)
	if err != nil || result.Complete || len(result.Findings) != 0 {
		t.Fatalf("unavailable target = %+v, %v", result, err)
	}
	base.TargetReady = true
	base.Sources = []BusinessObligation{{Key: "a"}, {Key: "b"}}
	if _, err := CompareBusinessObligations(base); err == nil {
		t.Fatal("over-limit run accepted")
	}
}
