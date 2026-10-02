package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
	"github.com/gin-gonic/gin"
)

// ConfigurationCheckResult is the read-only contract used by conversational
// clients before they ask the Engine to create a draft ConfigVersion.
type ConfigurationCheckResult struct {
	Valid             bool                      `json:"valid"`
	OrderedDocTypes   []string                  `json:"ordered_doctypes"`
	BaseConfigVersion string                    `json:"base_config_version_id,omitempty"`
	Errors            []string                  `json:"errors,omitempty"`
	Warnings          []string                  `json:"warnings,omitempty"`
	Conflicts         []ConfigurationConflict   `json:"conflicts,omitempty"`
	Impact            []ConfigurationImpactItem `json:"impact,omitempty"`
}

type ConfigurationConflict struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

type ConfigurationImpactItem struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Action string `json:"action"`
	Safe   bool   `json:"safe"`
}

// classifyCandidateDocType compares a generated candidate with the live
// registry. Identical definitions are a safe reuse; changed definitions are
// explicit updates that must remain blocked until the Engine's review path
// handles the diff.
func classifyCandidateDocType(reg *doctype.Registry, candidate *doctype.DocType) (string, bool, *ConfigurationConflict) {
	if reg == nil || !reg.Has(candidate.Name) {
		return "create", true, nil
	}
	existing := reg.Get(candidate.Name)
	diff := doctype.DiffConfigs([]*doctype.DocType{existing}, []*doctype.DocType{candidate})
	if len(diff.Changes) == 0 {
		return "reuse", true, nil
	}
	return "update", !diff.IsBreaking, &ConfigurationConflict{
		Kind: "doctype", Name: candidate.Name,
		Detail: fmt.Sprintf("the existing DocType differs from the candidate: %s", diff.Summary()),
	}
}

// HandleConfigurationValidate checks a complete candidate without writing a
// version, live table, migration, or external side effect.
func (h *Handler) HandleConfigurationValidate(c *gin.Context) {
	handleConfigurationCheck(c, h, false)
}

// HandleConfigurationDryRun adds impact information to the same read-only
// check. It deliberately does not create a ConfigVersion.
func (h *Handler) HandleConfigurationDryRun(c *gin.Context) {
	handleConfigurationCheck(c, h, true)
}

func handleConfigurationCheck(c *gin.Context, h *Handler, includeImpact bool) {
	var req DraftConfigurationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+err.Error(), nil)
		return
	}
	result := ConfigurationCheckResult{Valid: true, OrderedDocTypes: []string{}, Errors: []string{}, Warnings: []string{}, Conflicts: []ConfigurationConflict{}, Impact: []ConfigurationImpactItem{}}
	if len(req.DocTypes) == 0 {
		result.Valid = false
		result.Errors = append(result.Errors, "at least one DocType is required")
		c.JSON(http.StatusOK, Response{Data: result})
		return
	}
	ordered, err := orderDraftDocTypes(req.DocTypes)
	if err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, err.Error())
		c.JSON(http.StatusOK, Response{Data: result})
		return
	}
	result.OrderedDocTypes = orderedDocTypeNames(ordered)
	reg := h.siteRegistry(c)
	for _, dt := range ordered {
		if err := dt.Validate(); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("DocType %q failed validation: %v", dt.Name, err))
		}
		action, safe, conflict := classifyCandidateDocType(reg, dt)
		if conflict != nil {
			result.Valid = false
			result.Conflicts = append(result.Conflicts, *conflict)
		}
		if includeImpact {
			result.Impact = append(result.Impact, ConfigurationImpactItem{Name: dt.Name, Kind: "doctype", Action: action, Safe: safe})
		}
	}

	store := configstore.NewStore(h.siteTx(c).DB, h.siteDialect(c))
	_, baseVersionID, loadErr := store.LoadDraftHeadSnapshot(c.GetString("site_name"))
	if loadErr == nil {
		result.BaseConfigVersion = baseVersionID
	}
	if req.BaseConfigVersionID != "" && result.BaseConfigVersion != "" && req.BaseConfigVersionID != result.BaseConfigVersion {
		result.Valid = false
		result.Errors = append(result.Errors, "proposal is based on a stale configuration")
	}

	roleNames := map[string]bool{}
	for _, role := range req.Roles {
		if role == nil || strings.TrimSpace(role.Name) == "" {
			result.Valid = false
			result.Errors = append(result.Errors, "every generated role must have a name")
			continue
		}
		roleNames[role.Name] = true
		if includeImpact {
			result.Impact = append(result.Impact, ConfigurationImpactItem{Name: role.Name, Kind: "role", Action: "add", Safe: true})
		}
	}
	docNames := map[string]bool{}
	for _, dt := range ordered {
		docNames[dt.Name] = true
	}
	for _, permission := range req.Permissions {
		if permission == nil || strings.TrimSpace(permission.Role) == "" || strings.TrimSpace(permission.Doctype) == "" {
			result.Valid = false
			result.Errors = append(result.Errors, "every permission must name a role and DocType")
			continue
		}
		if !roleNames[permission.Role] && permission.Role != doctype.AdminRole {
			result.Warnings = append(result.Warnings, fmt.Sprintf("permission role %q is not in the generated role set", permission.Role))
		}
		if !docNames[permission.Doctype] && (reg == nil || !reg.Has(permission.Doctype)) {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("permission references unknown DocType %q", permission.Doctype))
		}
	}
	for _, workflow := range req.Workflows {
		if workflow == nil || strings.TrimSpace(workflow.Name) == "" || strings.TrimSpace(workflow.DocumentType) == "" {
			result.Valid = false
			result.Errors = append(result.Errors, "every workflow must name a workflow and DocType")
			continue
		}
		if !docNames[workflow.DocumentType] && (reg == nil || !reg.Has(workflow.DocumentType)) {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("workflow %q references unknown DocType %q", workflow.Name, workflow.DocumentType))
		}
		states := map[string]bool{}
		for _, state := range workflow.States {
			if strings.TrimSpace(state.State) != "" {
				states[state.State] = true
			}
		}
		for _, transition := range workflow.Transitions {
			if strings.TrimSpace(transition.Action) == "" {
				result.Valid = false
				result.Errors = append(result.Errors, fmt.Sprintf("workflow %q has a transition without an action", workflow.Name))
				continue
			}
			if !states[transition.From] || !states[transition.To] {
				result.Valid = false
				result.Errors = append(result.Errors, fmt.Sprintf("workflow %q has a transition with an unknown state", workflow.Name))
			}
		}
	}
	for _, view := range req.Views {
		if view == nil {
			result.Valid = false
			result.Errors = append(result.Errors, "generated views cannot be null")
			continue
		}
		if err := view.Validate(); err != nil {
			result.Valid = false
			result.Errors = append(result.Errors, fmt.Sprintf("view %q failed validation: %v", view.Name, err))
		}
	}
	if err := validateScriptCandidates(h, req.Scripts); err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, err.Error())
	}
	if includeImpact {
		for _, candidate := range req.Scripts {
			if candidate != nil {
				result.Impact = append(result.Impact, ConfigurationImpactItem{Name: candidate.Name, Kind: "script", Action: "add inactive draft", Safe: true})
			}
		}
	}
	if includeImpact && reg != nil {
		for _, dt := range ordered {
			preview := schema.AnalyzeImpact(h.siteTx(c).DB, reg.Get(dt.Name), dt, reg, h.siteDialect(c))
			if preview != nil && len(preview.Blocked) > 0 {
				result.Valid = false
				result.Errors = append(result.Errors, fmt.Sprintf("DocType %q has blocked impact", dt.Name))
			}
		}
	}
	c.JSON(http.StatusOK, Response{Data: result})
}
