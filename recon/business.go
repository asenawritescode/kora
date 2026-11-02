package recon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// BusinessObligation is supplied by the owning domain. Amounts are decimal
// strings so reconciliation never introduces binary floating point rounding.
type BusinessObligation struct {
	Key        string    `json:"key"`
	SourceRef  string    `json:"source_ref"`
	EntityID   string    `json:"entity_id"`
	Amount     string    `json:"amount"`
	Currency   string    `json:"currency"`
	OccurredAt time.Time `json:"occurred_at"`
	Lifecycle  string    `json:"lifecycle,omitempty"`
}

// BusinessEffect is an owner-query result representing an actual downstream
// business effect, independent of transport delivery receipts.
type BusinessEffect struct {
	Key        string    `json:"key"`
	TargetRef  string    `json:"target_ref"`
	EntityID   string    `json:"entity_id"`
	Amount     string    `json:"amount"`
	Currency   string    `json:"currency"`
	OccurredAt time.Time `json:"occurred_at"`
	Lifecycle  string    `json:"lifecycle,omitempty"`
}

// BusinessReconciliationRule binds a source and target to owner-published
// queries. The query executor is supplied by the existing module binding plan,
// so reconciliation never opens owner tables or introduces another registry.
type BusinessReconciliationRule struct {
	OwnerModuleID contract.ModuleID
	ID            string
	Version       uint32
	Scope         contract.BusinessScope
	SourceQuery   BusinessQueryRef
	TargetQuery   BusinessQueryRef
	GracePeriod   time.Duration
	MaxRows       int
}

type BusinessQueryRef struct {
	OwnerModuleID contract.ModuleID
	Name          string
	Input         json.RawMessage
	PageLimit     uint16
}

type BusinessQueryRequest struct {
	Scope         contract.BusinessScope
	OwnerModuleID contract.ModuleID
	Name          string
	Input         json.RawMessage
	Cursor        string
	Limit         uint16
}

// BusinessQueryPageReader adapts the existing published-query executor. A
// caller can implement this with BindingPlan.ExecutePublishedQuery.
type BusinessQueryPageReader func(context.Context, BusinessQueryRequest) (contract.QueryResult, error)

// PublishedBusinessQueryPlan is the existing module binding-plan query
// surface. It is intentionally the only provider lookup used by this runner.
type PublishedBusinessQueryPlan interface {
	ExecutePublishedQuery(context.Context, contract.BusinessScope, contract.ModuleID, string, json.RawMessage, string, uint16) (contract.QueryResult, error)
}

func PublishedBusinessQueryReader(plan PublishedBusinessQueryPlan) BusinessQueryPageReader {
	return func(ctx context.Context, request BusinessQueryRequest) (contract.QueryResult, error) {
		if plan == nil {
			return contract.QueryResult{}, fmt.Errorf("published module query plan is unavailable")
		}
		return plan.ExecutePublishedQuery(ctx, request.Scope, request.OwnerModuleID, request.Name, request.Input, request.Cursor, request.Limit)
	}
}

type BusinessRunCursor struct {
	Source         string                `json:"source,omitempty"`
	Target         string                `json:"target,omitempty"`
	SourceRows     []BusinessObligation  `json:"source_rows,omitempty"`
	TargetRows     []BusinessEffect      `json:"target_rows,omitempty"`
	Watermarks     BusinessRunWatermarks `json:"watermarks,omitempty"`
	SourceComplete bool                  `json:"source_complete,omitempty"`
	TargetComplete bool                  `json:"target_complete,omitempty"`
}

type BusinessRunWatermarks struct {
	SourceRevision string    `json:"source_revision,omitempty"`
	TargetRevision string    `json:"target_revision,omitempty"`
	SourceSnapshot string    `json:"source_snapshot,omitempty"`
	TargetSnapshot string    `json:"target_snapshot,omitempty"`
	SourceAsOf     time.Time `json:"source_as_of,omitempty"`
	TargetAsOf     time.Time `json:"target_as_of,omitempty"`
}

var businessDecimalPattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

// ExecuteBusinessReconciliation reads only declared owner queries, with
// bounded pagination and pinned per-query watermarks. Any unavailable,
// incomplete, stale, or inconsistent page yields an incomplete run.
func ExecuteBusinessReconciliation(ctx context.Context, rule BusinessReconciliationRule, cutoff time.Time, cursors BusinessRunCursor, read BusinessQueryPageReader) (BusinessReconciliationResult, BusinessRunCursor, BusinessRunWatermarks, error) {
	if ctx == nil || read == nil || rule.ID == "" || rule.Version == 0 || rule.OwnerModuleID == "" || rule.Scope.Validate() != nil || rule.GracePeriod < 0 || rule.MaxRows < 1 || rule.MaxRows > 10000 || cutoff.IsZero() {
		return BusinessReconciliationResult{}, cursors, BusinessRunWatermarks{}, fmt.Errorf("business reconciliation rule, scope, cutoff, or bounds are invalid")
	}
	if !validBusinessQueryRef(rule.SourceQuery) || !validBusinessQueryRef(rule.TargetQuery) {
		return BusinessReconciliationResult{}, cursors, BusinessRunWatermarks{}, fmt.Errorf("business reconciliation requires owner-published source and target queries")
	}
	if len(cursors.SourceRows) > 10000 || len(cursors.TargetRows) > 10000 {
		return BusinessReconciliationResult{}, cursors, BusinessRunWatermarks{}, fmt.Errorf("business reconciliation continuation exceeds accumulated row bound")
	}
	if cursors.Source != "" && (len(cursors.SourceRows) == 0 || cursors.Watermarks.SourceSnapshot == "" || cursors.Watermarks.SourceRevision == "") ||
		cursors.Target != "" && (len(cursors.TargetRows) == 0 || cursors.Watermarks.TargetSnapshot == "" || cursors.Watermarks.TargetRevision == "") {
		return BusinessReconciliationResult{Complete: false}, cursors, cursors.Watermarks, nil
	}
	sourceWatermark := businessPageWatermark{SourceRevision: cursors.Watermarks.SourceRevision, SnapshotToken: cursors.Watermarks.SourceSnapshot, AsOf: cursors.Watermarks.SourceAsOf}
	targetWatermark := businessPageWatermark{SourceRevision: cursors.Watermarks.TargetRevision, SnapshotToken: cursors.Watermarks.TargetSnapshot, AsOf: cursors.Watermarks.TargetAsOf}
	sources, sourceCursor, sourceComplete := cursors.SourceRows, cursors.Source, cursors.SourceComplete
	var err error
	if !sourceComplete {
		sources, sourceCursor, sourceWatermark, sourceComplete, err = readBusinessPages(ctx, rule, rule.SourceQuery, sourceCursor, sources, sourceWatermark, read)
	}
	if err != nil {
		return BusinessReconciliationResult{Complete: false}, businessCursor(sources, cursors.TargetRows, sourceCursor, cursors.Target, sourceWatermark, targetWatermark, false, cursors.TargetComplete), businessRunWatermarks(sourceWatermark, targetWatermark), nil
	}
	targets, targetCursor, targetComplete := cursors.TargetRows, cursors.Target, cursors.TargetComplete
	if !targetComplete {
		targets, targetCursor, targetWatermark, targetComplete, err = readBusinessPages(ctx, rule, rule.TargetQuery, targetCursor, targets, targetWatermark, read)
	}
	if err != nil {
		return BusinessReconciliationResult{Complete: false}, businessCursor(sources, targets, sourceCursor, targetCursor, sourceWatermark, targetWatermark, sourceComplete, false), businessRunWatermarks(sourceWatermark, targetWatermark), nil
	}
	watermarks := businessRunWatermarks(sourceWatermark, targetWatermark)
	if !sourceComplete || !targetComplete {
		return BusinessReconciliationResult{Complete: false}, businessCursor(sources, targets, sourceCursor, targetCursor, sourceWatermark, targetWatermark, sourceComplete, targetComplete), watermarks, nil
	}
	result, err := CompareBusinessObligations(BusinessReconciliationInput{Cutoff: cutoff, GracePeriod: rule.GracePeriod, MaxRows: 10000, Sources: sources, Targets: targets, SourceReady: true, TargetReady: true})
	return result, businessCursor(sources, targets, sourceCursor, targetCursor, sourceWatermark, targetWatermark, sourceComplete, targetComplete), watermarks, err
}

func businessRunWatermarks(source, target businessPageWatermark) BusinessRunWatermarks {
	return BusinessRunWatermarks{SourceRevision: source.SourceRevision, SourceSnapshot: source.SnapshotToken, SourceAsOf: source.AsOf, TargetRevision: target.SourceRevision, TargetSnapshot: target.SnapshotToken, TargetAsOf: target.AsOf}
}

func businessCursor(sources []BusinessObligation, targets []BusinessEffect, source, target string, sourceMark, targetMark businessPageWatermark, sourceComplete, targetComplete bool) BusinessRunCursor {
	return BusinessRunCursor{Source: source, Target: target, SourceRows: sources, TargetRows: targets, Watermarks: businessRunWatermarks(sourceMark, targetMark), SourceComplete: sourceComplete, TargetComplete: targetComplete}
}

func validBusinessQueryRef(ref BusinessQueryRef) bool {
	return ref.OwnerModuleID != "" && ref.Name != "" && ref.PageLimit > 0 && ref.PageLimit <= 1000 && len(ref.Input) > 0 && len(ref.Input) <= 65536 && json.Valid(ref.Input)
}

type businessPageWatermark struct {
	SourceRevision string
	SnapshotToken  string
	AsOf           time.Time
}

func readBusinessPages[T BusinessObligation | BusinessEffect](ctx context.Context, rule BusinessReconciliationRule, ref BusinessQueryRef, cursor string, prior []T, watermark businessPageWatermark, read BusinessQueryPageReader) ([]T, string, businessPageWatermark, bool, error) {
	rows := append([]T(nil), prior...)
	added := 0
	seenCursors := map[string]bool{}
	for added < rule.MaxRows && len(rows) < 10000 {
		limit := min(rule.MaxRows-added, 10000-len(rows), int(ref.PageLimit))
		page, err := read(ctx, BusinessQueryRequest{Scope: rule.Scope, OwnerModuleID: ref.OwnerModuleID, Name: ref.Name, Input: append(json.RawMessage(nil), ref.Input...), Cursor: cursor, Limit: uint16(limit)})
		if err != nil {
			return rows, cursor, watermark, false, err
		}
		if len(page.Data) == 0 || page.AsOf.IsZero() || page.Freshness != contract.QueryFresh || page.NextCursor == cursor && page.NextCursor != "" {
			return rows, cursor, watermark, false, nil
		}
		if watermark.AsOf.IsZero() {
			watermark = businessPageWatermark{SourceRevision: page.SourceRevision, SnapshotToken: page.SnapshotToken, AsOf: page.AsOf}
		} else if watermark.SourceRevision != page.SourceRevision || watermark.SnapshotToken != page.SnapshotToken || !watermark.AsOf.Equal(page.AsOf) {
			return rows, cursor, watermark, false, nil
		}
		if page.NextCursor != "" && (page.SnapshotToken == "" || page.SourceRevision == "") {
			return rows, cursor, watermark, false, nil
		}
		var pageRows []json.RawMessage
		if err := json.Unmarshal(page.Data, &pageRows); err != nil || len(pageRows) > limit {
			return rows, cursor, watermark, false, fmt.Errorf("business query returned invalid or unbounded rows")
		}
		for _, raw := range pageRows {
			var row T
			if err := json.Unmarshal(raw, &row); err != nil {
				return rows, cursor, watermark, false, err
			}
			rows = append(rows, row)
			added++
		}
		if len(pageRows) == 0 && page.NextCursor != "" {
			return rows, cursor, watermark, false, nil
		}
		cursor = page.NextCursor
		if cursor == "" {
			return rows, "", watermark, true, nil
		}
		if seenCursors[cursor] {
			return rows, cursor, watermark, false, nil
		}
		seenCursors[cursor] = true
	}
	return rows, cursor, watermark, cursor == "", nil
}

type BusinessFindingKind string

const (
	FindingMissingEffect     BusinessFindingKind = "missing_effect"
	FindingDuplicateEffect   BusinessFindingKind = "duplicate_effect"
	FindingAmountMismatch    BusinessFindingKind = "amount_mismatch"
	FindingScopeMismatch     BusinessFindingKind = "scope_mismatch"
	FindingUnknownOutcome    BusinessFindingKind = "unknown_outcome"
	FindingPendingGrace      BusinessFindingKind = "pending_grace"
	FindingLifecycleMismatch BusinessFindingKind = "lifecycle_mismatch"
)

type BusinessFinding struct {
	ID         string
	Kind       BusinessFindingKind
	Key        string
	SourceRef  string
	TargetRefs []string
	Expected   BusinessObligation
	Actual     []BusinessEffect
	State      BusinessFindingState
	Evidence   string
}

type BusinessFindingState string

const (
	BusinessFindingOpen          BusinessFindingState = "open"
	BusinessFindingInvestigating BusinessFindingState = "investigating"
	BusinessFindingResolved      BusinessFindingState = "resolved"
	BusinessFindingWaived        BusinessFindingState = "waived"
)

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
	eligibleSources := sources[:0]
	for _, source := range sources {
		if source.OccurredAt.IsZero() {
			return BusinessReconciliationResult{}, fmt.Errorf("business obligation %q has no occurrence time", source.Key)
		}
		if !source.OccurredAt.After(input.Cutoff) {
			eligibleSources = append(eligibleSources, source)
		}
	}
	sources = eligibleSources
	eligibleTargets := targets[:0]
	for _, target := range targets {
		if target.OccurredAt.IsZero() {
			return BusinessReconciliationResult{}, fmt.Errorf("business effect %q has no occurrence time", target.TargetRef)
		}
		if !target.OccurredAt.After(input.Cutoff) {
			eligibleTargets = append(eligibleTargets, target)
		}
	}
	targets = eligibleTargets
	sort.Slice(sources, func(i, j int) bool { return sources[i].Key < sources[j].Key })
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Key != targets[j].Key {
			return targets[i].Key < targets[j].Key
		}
		return targets[i].TargetRef < targets[j].TargetRef
	})
	byKey := make(map[string][]BusinessEffect, len(targets))
	for _, effect := range targets {
		if effect.Key == "" || effect.TargetRef == "" || effect.EntityID == "" || effect.Currency == "" || effect.Amount == "" {
			return BusinessReconciliationResult{}, fmt.Errorf("business effect is missing stable key, target, entity, amount, or currency")
		}
		if _, ok := parseBusinessDecimal(effect.Amount); !ok {
			return BusinessReconciliationResult{}, fmt.Errorf("business effect %q has an invalid decimal amount", effect.TargetRef)
		}
		byKey[effect.Key] = append(byKey[effect.Key], effect)
	}
	seen := map[string]bool{}
	for _, source := range sources {
		if source.Key == "" || source.EntityID == "" || source.Currency == "" || source.Amount == "" {
			return BusinessReconciliationResult{}, fmt.Errorf("business obligation is missing stable key, entity, amount, or currency")
		}
		if _, ok := parseBusinessDecimal(source.Amount); !ok {
			return BusinessReconciliationResult{}, fmt.Errorf("business obligation %q has an invalid decimal amount", source.Key)
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
			case !businessDecimalEqual(effect.Amount, source.Amount) || effect.Currency != source.Currency:
				finding.Kind = FindingAmountMismatch
			case effect.Lifecycle == "unknown":
				finding.Kind = FindingUnknownOutcome
			case source.Lifecycle != "" && effect.Lifecycle != source.Lifecycle:
				finding.Kind = FindingLifecycleMismatch
			default:
				continue
			}
		}
		finding.ID = stableBusinessFindingID(finding.Kind, source.Key)
		finding.State = BusinessFindingOpen
		result.Findings = append(result.Findings, finding)
	}
	for key, effects := range byKey {
		if len(effects) > 0 && !seen[key] {
			finding := BusinessFinding{Kind: FindingDuplicateEffect, Key: key, Actual: append([]BusinessEffect(nil), effects...)}
			for _, effect := range effects {
				finding.TargetRefs = append(finding.TargetRefs, effect.TargetRef)
			}
			finding.ID = stableBusinessFindingID(finding.Kind, key)
			finding.State = BusinessFindingOpen
			result.Findings = append(result.Findings, finding)
		}
	}
	sort.Slice(result.Findings, func(i, j int) bool { return result.Findings[i].ID < result.Findings[j].ID })
	return result, nil
}

func businessDecimalEqual(a, b string) bool {
	left, leftOK := parseBusinessDecimal(a)
	right, rightOK := parseBusinessDecimal(b)
	return leftOK && rightOK && left.Cmp(right) == 0
}

func parseBusinessDecimal(value string) (*big.Rat, bool) {
	if len(value) > 64 || !businessDecimalPattern.MatchString(value) {
		return nil, false
	}
	amount, ok := new(big.Rat).SetString(value)
	return amount, ok
}

func validBusinessRunProgress(cursors BusinessRunCursor, watermarks BusinessRunWatermarks) bool {
	if len(cursors.SourceRows) > 10000 || len(cursors.TargetRows) > 10000 {
		return false
	}
	payload, err := json.Marshal(cursors)
	return err == nil && len(payload) <= 16<<20 && len(cursors.Source) <= 2048 && len(cursors.Target) <= 2048 &&
		len(watermarks.SourceRevision) <= 256 && len(watermarks.TargetRevision) <= 256 &&
		len(watermarks.SourceSnapshot) <= 2048 && len(watermarks.TargetSnapshot) <= 2048 &&
		len(cursors.Watermarks.SourceRevision) <= 256 && len(cursors.Watermarks.TargetRevision) <= 256 &&
		len(cursors.Watermarks.SourceSnapshot) <= 2048 && len(cursors.Watermarks.TargetSnapshot) <= 2048
}

func validBusinessFindingState(state BusinessFindingState) bool {
	switch state {
	case BusinessFindingOpen, BusinessFindingInvestigating, BusinessFindingResolved, BusinessFindingWaived:
		return true
	default:
		return false
	}
}

func validBusinessFindingKind(kind BusinessFindingKind) bool {
	switch kind {
	case FindingMissingEffect, FindingDuplicateEffect, FindingAmountMismatch, FindingScopeMismatch, FindingUnknownOutcome, FindingPendingGrace, FindingLifecycleMismatch:
		return true
	default:
		return false
	}
}

func stableBusinessFindingID(kind BusinessFindingKind, key string) string {
	hash := sha256.Sum256([]byte(string(kind) + "\x00" + key))
	return "bf_" + hex.EncodeToString(hash[:16])
}
