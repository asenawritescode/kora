package doctype

import (
	"fmt"
)

// ImpactTier classifies the safety of a schema change.
// The integer values are ordered: Safe (0) < Warning (1) < Blocked (2),
// so comparison operators work correctly.
type ImpactTier int

const (
	TierSafe    ImpactTier = iota // 0
	TierWarning                   // 1
	TierBlocked                   // 2
)

func (t ImpactTier) String() string {
	switch t {
	case TierSafe:
		return "safe"
	case TierWarning:
		return "warning"
	case TierBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// ImpactResult contains the classified impact analysis for a set of changes.
type ImpactResult struct {
	Tier    ImpactTier
	Changes []Change
	Summary string
}

// impactRule describes how to classify a given change type.
type impactRule struct {
	Tier      ImpactTier
	Condition func(Change) bool // optional: additional condition for escalation
}

// impactLookup is the structural lookup table that classifies each change type
// by its safety tier. This replaces the procedural comparisons in schema/migrator.go.
var impactLookup = map[string]impactRule{
	"add-doctype":         {Tier: TierSafe},
	"remove-doctype":      {Tier: TierWarning},
	"add-field":           {Tier: TierSafe, Condition: addFieldIsWarning},
	"remove-field":        {Tier: TierWarning},
	"rename-field":        {Tier: TierSafe},
	"change-field-type":   {Tier: TierBlocked},
	"change-field-property": {Tier: TierSafe, Condition: changePropertyIsWarning},
	"add-role":            {Tier: TierSafe},
	"remove-role":         {Tier: TierWarning},
	"add-perm":            {Tier: TierSafe},
	"remove-perm":         {Tier: TierSafe},
	"add-workflow":        {Tier: TierSafe},
	"remove-workflow":     {Tier: TierWarning},
	"add-metric":          {Tier: TierSafe},
	"remove-metric":       {Tier: TierSafe},
	"add-script-ref":      {Tier: TierSafe},
	"remove-script-ref":   {Tier: TierSafe},
}

// addFieldIsWarning returns true if adding this field should be classified as
// a warning — specifically when a required field has no default value.
func addFieldIsWarning(c Change) bool {
	reqd, ok := c.Attrs[":required"]
	if !ok {
		return false
	}
	b, ok := reqd.(bool)
	if !ok || !b {
		return false
	}
	def, hasDefault := c.Attrs[":default"]
	if !hasDefault {
		return true
	}
	s, ok := def.(string)
	return !ok || s == ""
}

// changePropertyIsWarning returns true if changing a field property should be
// classified as a warning — making required or adding constraints.
func changePropertyIsWarning(c Change) bool {
	if v, ok := c.Attrs[":required"]; ok {
		if b, ok := v.(bool); ok && b {
			return true
		}
	}
	if _, ok := c.Attrs[":constraints"]; ok {
		return true
	}
	return false
}

// AnalyzeImpactFromChanges classifies a change list using a structural lookup table.
// Each change is evaluated against the lookup table. Unknown change types default
// to TierWarning. The result contains the worst tier found across all changes.
func AnalyzeImpactFromChanges(changes []Change) *ImpactResult {
	result := &ImpactResult{Tier: TierSafe}

	worstTier := TierSafe
	hasBlocked := false
	hasWarning := false

	for _, c := range changes {
		rule, exists := impactLookup[c.Type]

		// Apply the rule or default to warning for unknown types.
		tier := TierWarning
		if exists {
			tier = rule.Tier
			if rule.Condition != nil && rule.Condition(c) {
				tier = TierWarning
			}
		}

		switch tier {
		case TierBlocked:
			hasBlocked = true
		case TierWarning:
			hasWarning = true
		}

		if tier > worstTier {
			worstTier = tier
		}
	}

	result.Tier = worstTier
	result.Changes = changes
	result.Summary = summarizeImpact(hasBlocked, hasWarning, len(changes))
	return result
}

// summarizeImpact produces a human-readable summary string.
func summarizeImpact(hasBlocked, hasWarning bool, total int) string {
	if total == 0 {
		return "No changes detected"
	}
	parts := fmt.Sprintf("%d change(s)", total)
	if hasBlocked {
		parts += ", includes BLOCKED changes"
	} else if hasWarning {
		parts += ", includes warnings"
	} else {
		parts += ", all safe"
	}
	return parts
}
