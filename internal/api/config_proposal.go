package api

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/gin-gonic/gin"
)

// DraftConfigurationRequest is Engine's domain-typed view of the generated
// configuration-draft wire contract. The complete package is composed into
// one draft; callers must not create each DocType independently.
type DraftConfigurationRequest struct {
	ProposalID           string                                 `json:"proposal_id"`
	DraftConfigVersionID string                                 `json:"draft_config_version_id,omitempty"`
	ConversationID       string                                 `json:"conversation_id,omitempty"`
	InterviewSnapshot    map[string]any                         `json:"interview_snapshot,omitempty"`
	PrimaryWorkflow      string                                 `json:"primary_workflow,omitempty"`
	StructuredInput      map[string]any                         `json:"structured_input,omitempty"`
	OpenQuestions        []string                               `json:"open_questions,omitempty"`
	ChannelOrigin        string                                 `json:"channel_origin,omitempty"`
	BaseConfigVersionID  string                                 `json:"base_config_version_id,omitempty"`
	CreatedBy            string                                 `json:"created_by,omitempty"` // legacy input; attribution uses verified execution
	DocTypes             []*doctype.DocType                     `json:"doctypes"`
	Roles                []*doctype.Role                        `json:"roles,omitempty"`
	Permissions          []*DraftPermission                     `json:"permissions,omitempty"`
	Views                []*doctype.View                        `json:"views,omitempty"`
	Scripts              []*ScriptCandidate                     `json:"scripts,omitempty"`
	ScheduledJobs        []contract.ScheduledJobDefinition      `json:"scheduled_jobs,omitempty"`
	WorkflowDefinitions  []contract.WorkflowDefinition          `json:"workflow_definitions,omitempty"`
	PublishedBindings    []contract.ConfigurationSurfaceBinding `json:"published_bindings,omitempty"`
}

// DraftPermission includes the legacy public flag so the Engine can reject an
// unsupported access grant instead of silently dropping it during decoding.
type DraftPermission struct {
	doctype.Permission
	Public bool `json:"public,omitempty"`
}

type ScriptCandidate struct {
	Name       string `json:"name"`
	ScriptType string `json:"script_type"`
	DocType    string `json:"doctype,omitempty"`
	Event      string `json:"event,omitempty"`
	MethodPath string `json:"method_path,omitempty"`
	Schedule   string `json:"schedule,omitempty"`
	RunAs      string `json:"run_as,omitempty"`
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
	Source     string `json:"source"`
}

// HandleConfigurationDraft composes an ordered, validated package into one
// Draft ConfigVersion. It never writes live DocType tables or activates code.
func (h *Handler) HandleConfigurationDraft(c *gin.Context) {
	req, ok := decodeDraftConfigurationRequest(c)
	if !ok {
		return
	}
	if strings.TrimSpace(req.ProposalID) == "" {
		badRequestError(c, "proposal.required", "proposal_id is required", nil)
		return
	}
	if err := contract.ValidateScheduledJobDefinitions(req.ScheduledJobs); err != nil {
		badRequestError(c, "proposal.invalid_scheduled_jobs", err.Error(), nil)
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
			// Conversational clients may include existing definitions in a
			// complete candidate so the draft snapshot remains self-contained.
			// Reusing an identical definition is safe; changed definitions still
			// require the explicit schema-review path and must not be smuggled in
			// as a create operation.
			_, _, conflict := classifyCandidateDocType(reg, dt)
			if conflict != nil {
				conflictError(c, "doctype.already_exists", conflict.Detail, map[string]any{"doctype": dt.Name})
				return
			}
		}
	}
	if issues := validateWorkflowDefinitionCandidates(h, c, req.WorkflowDefinitions, reg, ordered); len(issues) > 0 {
		badRequestError(c, "proposal.invalid_workflow_definitions", strings.Join(issues, "; "), nil)
		return
	}
	if messages, _ := validateConfigurationCandidateRules(req, ordered, reg, h); len(messages) > 0 {
		badRequestError(c, "proposal.invalid_configuration", strings.Join(messages, "; "), nil)
		return
	}
	_, moduleBlockers, moduleErrors := validateConfigurationPublishedBindings(h, c, req.PublishedBindings, req.Views)
	if len(moduleErrors) > 0 || len(moduleBlockers) > 0 {
		badRequestError(c, "proposal.invalid_published_bindings", "Published module bindings are stale or unavailable", map[string]any{"blockers": moduleBlockers, "errors": moduleErrors})
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
	snapshot.PublishedBindings = append([]contract.ConfigurationSurfaceBinding(nil), req.PublishedBindings...)
	for _, dt := range ordered {
		doctype.UpsertSnapshotDocType(snapshot, dt)
	}
	for _, role := range req.Roles {
		upsertRole(snapshot, role)
	}
	for _, permission := range req.Permissions {
		if permission != nil && permission.Public {
			badRequestError(c, "proposal.public_permission_unsupported", "Public permission grants are unsupported; configure an explicit reviewed public-access policy", nil)
			return
		}
		upsertPermission(snapshot, &permission.Permission)
	}
	for _, view := range req.Views {
		upsertView(snapshot, view)
	}
	for _, candidate := range req.Scripts {
		hash := sha256.Sum256([]byte(candidate.Source))
		snapshot.Scripts = append(snapshot.Scripts, &doctype.ScriptSnapshot{
			Name: candidate.Name, ScriptType: candidate.ScriptType, DocType: candidate.DocType,
			Event: candidate.Event, MethodPath: candidate.MethodPath,
			Schedule: candidate.Schedule, Priority: 10, IsActive: false, RunAs: candidate.RunAs,
			TimeoutMs: candidate.TimeoutMs, ScriptHash: fmt.Sprintf("%x", hash[:]), Source: candidate.Source,
		})
	}
	for _, definition := range req.WorkflowDefinitions {
		upsertWorkflowDefinition(snapshot, definition)
	}
	if len(req.ScheduledJobs) > 0 {
		byName := make(map[string]contract.ScheduledJobDefinition, len(snapshot.ScheduledJobs)+len(req.ScheduledJobs))
		for _, job := range snapshot.ScheduledJobs {
			byName[job.Name] = job
		}
		for _, job := range req.ScheduledJobs {
			byName[job.Name] = job
		}
		snapshot.ScheduledJobs = snapshot.ScheduledJobs[:0]
		for _, job := range byName {
			snapshot.ScheduledJobs = append(snapshot.ScheduledJobs, job)
		}
		sort.Slice(snapshot.ScheduledJobs, func(i, j int) bool { return snapshot.ScheduledJobs[i].Name < snapshot.ScheduledJobs[j].Name })
	}
	if rejectInvalidSnapshotReferences(c, snapshot, reg) {
		return
	}

	createdBy := currentUser(c)
	label := configurationDraftLabel(req)
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
	c.JSON(http.StatusOK, Response{Data: contract.ConfigurationDraftResultV2{
		ProposalID: req.ProposalID, ConversationID: req.ConversationID,
		DraftConfigVersionID: versionID, VersionNum: versionNum,
		Status: "Draft", OrderedDocTypes: orderedDocTypeNames(ordered),
	}})
}

func configurationDraftLabel(req DraftConfigurationRequest) string {
	context, _ := req.InterviewSnapshot["industry"].(string)
	context = strings.Join(strings.Fields(strings.TrimSpace(context)), " ")
	if context == "" {
		context = strings.Join(strings.Fields(strings.TrimSpace(req.PrimaryWorkflow)), " ")
	}
	if context == "" {
		return "Workspace setup draft"
	}
	runes := []rune(context)
	if len(runes) > 52 {
		context = strings.TrimSpace(string(runes[:52])) + "…"
	}
	return context + " setup draft"
}

func upsertWorkflowDefinition(snapshot *doctype.ConfigSnapshot, definition contract.WorkflowDefinition) {
	if snapshot == nil {
		return
	}
	for i := range snapshot.WorkflowDefinitions {
		if snapshot.WorkflowDefinitions[i].ID == definition.ID {
			snapshot.WorkflowDefinitions[i] = definition
			return
		}
	}
	snapshot.WorkflowDefinitions = append(snapshot.WorkflowDefinitions, definition)
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
		case script.TypeDocEvent, script.TypeAPIMethod, script.TypeScheduled:
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
