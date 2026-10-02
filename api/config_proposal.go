package api

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/script"
	"github.com/gin-gonic/gin"
)

// DraftConfigurationRequest is the Engine boundary for Cloud-generated
// onboarding configuration. The complete package is composed into one draft;
// callers must not create each DocType independently.
type DraftConfigurationRequest struct {
	ProposalID           string                `json:"proposal_id"`
	DraftConfigVersionID string                `json:"draft_config_version_id,omitempty"`
	ConversationID       string                `json:"conversation_id,omitempty"`
	InterviewSnapshot    map[string]any        `json:"interview_snapshot,omitempty"`
	PrimaryWorkflow      string                `json:"primary_workflow,omitempty"`
	StructuredInput      map[string]any        `json:"structured_input,omitempty"`
	OpenQuestions        []string              `json:"open_questions,omitempty"`
	ChannelOrigin        string                `json:"channel_origin,omitempty"`
	BaseConfigVersionID  string                `json:"base_config_version_id,omitempty"`
	CreatedBy            string                `json:"created_by,omitempty"`
	DocTypes             []*doctype.DocType    `json:"doctypes"`
	Roles                []*doctype.Role       `json:"roles,omitempty"`
	Permissions          []*doctype.Permission `json:"permissions,omitempty"`
	Workflows            []*doctype.Workflow   `json:"workflows,omitempty"`
	Views                []*doctype.View       `json:"views,omitempty"`
	Scripts              []*ScriptCandidate    `json:"scripts,omitempty"`
}

type ScriptCandidate struct {
	Name           string `json:"name"`
	ScriptType     string `json:"script_type"`
	DocType        string `json:"doctype,omitempty"`
	Event          string `json:"event,omitempty"`
	MethodPath     string `json:"method_path,omitempty"`
	WorkflowAction string `json:"workflow_action,omitempty"`
	Schedule       string `json:"schedule,omitempty"`
	RunAs          string `json:"run_as,omitempty"`
	TimeoutMs      int    `json:"timeout_ms,omitempty"`
	Source         string `json:"source"`
}

// HandleConfigurationDraft composes an ordered, validated package into one
// Draft ConfigVersion. It never writes live DocType tables or activates code.
func (h *Handler) HandleConfigurationDraft(c *gin.Context) {
	var req DraftConfigurationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.ProposalID) == "" {
		badRequestError(c, "proposal.required", "proposal_id is required", nil)
		return
	}
	if len(req.DocTypes) == 0 {
		badRequestError(c, "proposal.doctypes_required", "At least one DocType is required", nil)
		return
	}

	ordered, err := orderDraftDocTypes(req.DocTypes)
	if err != nil {
		c.JSON(http.StatusConflict, ErrorResponse{Error: map[string]string{"message": err.Error()}})
		return
	}

	reg := h.siteRegistry(c)
	for _, dt := range ordered {
		if err := dt.Validate(); err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{
				"message": fmt.Sprintf("DocType %q failed validation: %v", dt.Name, err),
			}})
			return
		}
		if reg != nil && reg.Has(dt.Name) {
			conflictError(c, "doctype.already_exists", "DocType already exists: "+dt.Name, map[string]any{"doctype": dt.Name})
			return
		}
	}
	for _, role := range req.Roles {
		if role == nil || strings.TrimSpace(role.Name) == "" {
			badRequestError(c, "proposal.invalid_role", "Every generated role must have a name", nil)
			return
		}
	}
	if err := validateScriptCandidates(h, req.Scripts); err != nil {
		badRequestError(c, "proposal.invalid_script", err.Error(), nil)
		return
	}

	store := configstore.NewStore(h.siteTx(c).DB, h.siteDialect(c))
	snapshot, baseVersionID, err := store.LoadDraftHeadSnapshot(c.GetString("site_name"))
	if err != nil {
		snapshot, err = store.CollectSnapshot(reg, c.GetString("site_name"))
		if err != nil {
			internalError(c, "collecting configuration snapshot", err)
			return
		}
		baseVersionID = ""
	}
	if req.BaseConfigVersionID != "" && baseVersionID != "" && req.BaseConfigVersionID != baseVersionID {
		c.JSON(http.StatusConflict, ErrorResponse{Error: map[string]string{"message": "proposal is based on a stale configuration"}})
		return
	}
	if req.BaseConfigVersionID != "" {
		baseVersionID = req.BaseConfigVersionID
	}
	for _, dt := range ordered {
		doctype.UpsertSnapshotDocType(snapshot, dt)
	}
	for _, role := range req.Roles {
		upsertRole(snapshot, role)
	}
	for _, permission := range req.Permissions {
		upsertPermission(snapshot, permission)
	}
	for _, workflow := range req.Workflows {
		upsertWorkflow(snapshot, workflow)
	}
	for _, view := range req.Views {
		upsertView(snapshot, view)
	}
	for _, candidate := range req.Scripts {
		hash := sha256.Sum256([]byte(candidate.Source))
		snapshot.Scripts = append(snapshot.Scripts, &doctype.ScriptSnapshot{
			Name: candidate.Name, ScriptType: candidate.ScriptType, DocType: candidate.DocType,
			Event: candidate.Event, MethodPath: candidate.MethodPath, WorkflowAction: candidate.WorkflowAction,
			Schedule: candidate.Schedule, Priority: 10, IsActive: false, RunAs: candidate.RunAs,
			TimeoutMs: candidate.TimeoutMs, ScriptHash: fmt.Sprintf("%x", hash[:]), Source: candidate.Source,
		})
	}

	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		createdBy = c.GetString("user")
	}
	if createdBy == "" {
		createdBy = "cloud-proposal"
	}
	label := "Proposal " + req.ProposalID + " configuration draft"
	versionID := req.DraftConfigVersionID
	versionNum := 0
	if versionID != "" {
		versionNum, err = store.UpdateDraftConfigVersion(c.GetString("site_name"), versionID, createdBy, label, snapshot, baseVersionID)
	} else {
		versionID, versionNum, err = store.CreateConfigVersionWithBase(c.GetString("site_name"), createdBy, label, "Draft", snapshot, baseVersionID)
	}
	if err != nil {
		internalError(c, "saving configuration draft", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"proposal_id":             req.ProposalID,
		"conversation_id":         req.ConversationID,
		"draft_config_version_id": versionID,
		"version_num":             versionNum,
		"status":                  "Draft",
		"ordered_doctypes":        orderedDocTypeNames(ordered),
	}})
}

func validateScriptCandidates(h *Handler, candidates []*ScriptCandidate) error {
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == nil || strings.TrimSpace(candidate.Name) == "" {
			return fmt.Errorf("every script must have a name")
		}
		if seen[strings.ToLower(strings.TrimSpace(candidate.Name))] {
			return fmt.Errorf("generated script names conflict: %q", candidate.Name)
		}
		seen[strings.ToLower(strings.TrimSpace(candidate.Name))] = true
		if strings.TrimSpace(candidate.Source) == "" {
			return fmt.Errorf("script %q has no source", candidate.Name)
		}
		switch script.Type(candidate.ScriptType) {
		case script.TypeDocEvent, script.TypeAPIMethod, script.TypeWorkflowAction, script.TypeScheduled:
		default:
			return fmt.Errorf("script %q has unsupported script_type %q", candidate.Name, candidate.ScriptType)
		}
		if script.Type(candidate.ScriptType) == script.TypeAPIMethod && strings.TrimSpace(candidate.MethodPath) == "" {
			return fmt.Errorf("API method script %q must have method_path", candidate.Name)
		}
		if h.ScriptRunner != nil {
			if err := h.ScriptRunner.Validate(candidate.Source); err != nil {
				return fmt.Errorf("script %q failed validation: %v", candidate.Name, err)
			}
		}
		lower := strings.ToLower(candidate.Source)
		for _, forbidden := range []string{"api_key", "apikey", "password", "bearer ", "authorization:", "private key", "child_process", "exec("} {
			if strings.Contains(lower, forbidden) {
				return fmt.Errorf("script %q contains a blocked credential or execution pattern", candidate.Name)
			}
		}
	}
	return nil
}

func upsertRole(snapshot *doctype.ConfigSnapshot, role *doctype.Role) {
	for i, existing := range snapshot.Roles {
		if existing != nil && existing.Name == role.Name {
			snapshot.Roles[i] = role
			return
		}
	}
	snapshot.Roles = append(snapshot.Roles, role)
}

func upsertPermission(snapshot *doctype.ConfigSnapshot, permission *doctype.Permission) {
	for i, existing := range snapshot.Permissions {
		if existing != nil && existing.Role == permission.Role && existing.Doctype == permission.Doctype {
			snapshot.Permissions[i] = permission
			return
		}
	}
	snapshot.Permissions = append(snapshot.Permissions, permission)
}

func upsertWorkflow(snapshot *doctype.ConfigSnapshot, workflow *doctype.Workflow) {
	for i, existing := range snapshot.Workflows {
		if existing != nil && existing.Name == workflow.Name {
			snapshot.Workflows[i] = workflow
			return
		}
	}
	snapshot.Workflows = append(snapshot.Workflows, workflow)
}

func upsertView(snapshot *doctype.ConfigSnapshot, view *doctype.View) {
	for i, existing := range snapshot.Views {
		if existing != nil && existing.Name == view.Name {
			snapshot.Views[i] = view
			return
		}
	}
	snapshot.Views = append(snapshot.Views, view)
}

func orderedDocTypeNames(items []*doctype.DocType) []string {
	result := make([]string, 0, len(items))
	for _, dt := range items {
		result = append(result, dt.Name)
	}
	return result
}

// orderDraftDocTypes performs a deterministic topological sort. Link targets
// and child-table targets are dependencies, so they are composed first.
func orderDraftDocTypes(input []*doctype.DocType) ([]*doctype.DocType, error) {
	byName := make(map[string]*doctype.DocType, len(input))
	canonical := make(map[string]string, len(input))
	for _, dt := range input {
		if dt == nil || strings.TrimSpace(dt.Name) == "" {
			return nil, fmt.Errorf("every generated DocType must have a name")
		}
		key := normalizeDocTypeName(dt.Name)
		if previous, exists := canonical[key]; exists {
			return nil, fmt.Errorf("generated DocType names conflict: %q and %q", previous, dt.Name)
		}
		canonical[key] = dt.Name
		byName[dt.Name] = dt
	}

	deps := make(map[string]map[string]bool, len(input))
	for _, dt := range input {
		deps[dt.Name] = map[string]bool{}
		for _, field := range dt.Fields {
			target := strings.TrimSpace(field.Options)
			if target == "" || (field.Fieldtype != "Link" && field.Fieldtype != "Table") {
				continue
			}
			if canonicalTarget, ok := canonical[normalizeDocTypeName(target)]; ok && canonicalTarget != dt.Name {
				deps[dt.Name][canonicalTarget] = true
			}
		}
	}

	ordered := make([]*doctype.DocType, 0, len(input))
	for len(ordered) < len(input) {
		progress := false
		for _, dt := range input {
			if byName[dt.Name] == nil {
				continue
			}
			ready := true
			for dependency := range deps[dt.Name] {
				if byName[dependency] != nil {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			ordered = append(ordered, dt)
			delete(byName, dt.Name)
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("generated DocType dependencies contain a cycle")
		}
		for _, dependencies := range deps {
			for dependency := range dependencies {
				if byName[dependency] == nil {
					delete(dependencies, dependency)
				}
			}
		}
	}
	return ordered, nil
}

func normalizeDocTypeName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
