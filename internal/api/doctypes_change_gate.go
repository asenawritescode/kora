package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/doctype"
)

// requireSafeDoctypeChange blocks live doctype mutations that are warning or blocked
// unless the caller explicitly opts in with force=true.
func requireSafeDoctypeChange(c *gin.Context, oldDocTypes, newDocTypes []*doctype.DocType) bool {
	if c.Query("force") == "true" {
		return true
	}

	diff := doctype.DiffConfigs(oldDocTypes, newDocTypes)
	impact := doctype.AnalyzeImpactFromChanges(
		doctype.ConvertConfigChanges(diff.Changes, nil, &doctype.ConfigSnapshot{DocTypes: newDocTypes}),
	)

	if impact.Tier == doctype.TierSafe {
		return true
	}

	writeError(c, http.StatusConflict, "doctype.change_requires_confirmation",
		"This change is destructive or potentially breaking. Re-run with force=true to apply it.",
		map[string]any{
			"impact_tier":    impact.Tier.String(),
			"impact_summary": impact.Summary,
			"is_breaking":    diff.IsBreaking,
			"diff_summary":   diff.Summary(),
			"changes":        impact.Changes,
			"force_query":    "force=true",
		},
	)
	return false
}

func singleDocTypeSlice(dt *doctype.DocType) []*doctype.DocType {
	if dt == nil {
		return nil
	}
	return []*doctype.DocType{dt}
}
