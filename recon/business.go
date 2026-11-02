package recon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// BusinessObligation is supplied by the owning domain. Amounts are decimal
// strings so reconciliation never introduces binary floating point rounding.
type BusinessObligation struct {
	Key        string
	SourceRef  string
	EntityID   string
	Amount     string
	Currency   string
	OccurredAt time.Time
	Lifecycle  string
}

// BusinessEffect is an owner-query result representing an actual downstream
// business effect, independent of transport delivery receipts.
type BusinessEffect struct {
	Key        string
	TargetRef  string
	EntityID   string
	Amount     string
	Currency   string
	OccurredAt time.Time
	Lifecycle  string
}

type BusinessFindingKind string

const (
	FindingMissingEffect   BusinessFindingKind = "missing_effect"
	FindingDuplicateEffect BusinessFindingKind = "duplicate_effect"
	FindingAmountMismatch  BusinessFindingKind = "amount_mismatch"
	FindingScopeMismatch   BusinessFindingKind = "scope_mismatch"
	FindingUnknownOutcome  BusinessFindingKind = "unknown_outcome"
	FindingPendingGrace    BusinessFindingKind = "pending_grace"
)

type BusinessFinding struct {
	ID         string
	Kind       BusinessFindingKind
	Key        string
	SourceRef  string
	TargetRefs []string
	Expected   BusinessObligation
	Actual     []BusinessEffect
}

type BusinessReconciliationInput struct {
	Cutoff      time.Time
	GracePeriod time.Duration
	MaxRows     int
	Sources     []BusinessObligation
	Targets     []BusinessEffect
	SourceReady bool
	TargetReady bool
}

type BusinessReconciliationResult struct {
	Complete bool
	Findings []BusinessFinding
}

// CompareBusinessObligations performs a bounded deterministic comparison over
// stable owner keys. It intentionally returns findings only; repairs must be
// separate authorized owner commands.
func CompareBusinessObligations(input BusinessReconciliationInput) (BusinessReconciliationResult, error) {
	if input.Cutoff.IsZero() || input.GracePeriod < 0 || input.MaxRows < 1 || input.MaxRows > 10000 {
		return BusinessReconciliationResult{}, fmt.Errorf("business reconciliation requires cutoff, nonnegative grace, and bounded row limit")
	}
	if len(input.Sources) > input.MaxRows || len(input.Targets) > input.MaxRows {
		return BusinessReconciliationResult{}, fmt.Errorf("business reconciliation row limit exceeded")
	}
	result := BusinessReconciliationResult{Complete: input.SourceReady && input.TargetReady}
	if !result.Complete {
		return result, nil
	}
	sources := append([]BusinessObligation(nil), input.Sources...)
	targets := append([]BusinessEffect(nil), input.Targets...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Key < sources[j].Key })
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Key != targets[j].Key {
			return targets[i].Key < targets[j].Key
		}
		return targets[i].TargetRef < targets[j].TargetRef
	})
	byKey := make(map[string][]BusinessEffect, len(targets))
	for _, effect := range targets {
		byKey[effect.Key] = append(byKey[effect.Key], effect)
	}
	seen := map[string]bool{}
	for _, source := range sources {
		if source.Key == "" || source.EntityID == "" || source.Currency == "" || source.Amount == "" {
			return BusinessReconciliationResult{}, fmt.Errorf("business obligation is missing stable key, entity, amount, or currency")
		}
		if seen[source.Key] {
			return BusinessReconciliationResult{}, fmt.Errorf("duplicate source obligation key %q", source.Key)
		}
		seen[source.Key] = true
		actual := byKey[source.Key]
		finding := BusinessFinding{Key: source.Key, SourceRef: source.SourceRef, Expected: source, Actual: append([]BusinessEffect(nil), actual...)}
		for _, effect := range actual {
			finding.TargetRefs = append(finding.TargetRefs, effect.TargetRef)
		}
		switch {
		case len(actual) == 0 && source.OccurredAt.Add(input.GracePeriod).After(input.Cutoff):
			finding.Kind = FindingPendingGrace
		case len(actual) == 0:
			finding.Kind = FindingMissingEffect
		case len(actual) > 1:
			finding.Kind = FindingDuplicateEffect
		default:
			effect := actual[0]
			switch {
			case effect.EntityID != source.EntityID:
				finding.Kind = FindingScopeMismatch
			case effect.Amount != source.Amount || effect.Currency != source.Currency:
				finding.Kind = FindingAmountMismatch
			case effect.Lifecycle == "unknown":
				finding.Kind = FindingUnknownOutcome
			default:
				continue
			}
		}
		finding.ID = stableBusinessFindingID(finding.Kind, source.Key)
		result.Findings = append(result.Findings, finding)
	}
	for key, effects := range byKey {
		if len(effects) > 0 && !seen[key] {
			finding := BusinessFinding{Kind: FindingDuplicateEffect, Key: key, Actual: append([]BusinessEffect(nil), effects...)}
			for _, effect := range effects {
				finding.TargetRefs = append(finding.TargetRefs, effect.TargetRef)
			}
			finding.ID = stableBusinessFindingID(finding.Kind, key)
			result.Findings = append(result.Findings, finding)
		}
	}
	sort.Slice(result.Findings, func(i, j int) bool { return result.Findings[i].ID < result.Findings[j].ID })
	return result, nil
}

func stableBusinessFindingID(kind BusinessFindingKind, key string) string {
	hash := sha256.Sum256([]byte(string(kind) + "\x00" + key))
	return "bf_" + hex.EncodeToString(hash[:16])
}
