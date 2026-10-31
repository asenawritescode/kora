package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
	flow "github.com/asenawritescode/kora/workflow"
	"github.com/gin-gonic/gin"
)

const configurationCheckBodyLimit = 3 << 20

// ConfigurationCheckResult is the read-only contract used by conversational
// clients before they ask the Engine to create a draft ConfigVersion.
type ConfigurationCheckResult = contract.ConfigurationCheckV2
type ConfigurationConflict = contract.ConfigurationConflict
type ConfigurationImpactItem = contract.ConfigurationImpact

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
	req, ok := decodeDraftConfigurationRequest(c)
	if !ok {
		return
	}
	var err error
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

	validationErrors, warnings := validateConfigurationCandidateRules(req, ordered, reg, h)
	result.Errors = append(result.Errors, validationErrors...)
	result.Warnings = append(result.Warnings, warnings...)
	result.Errors = append(result.Errors, validateWorkflowDefinitionCandidates(h, c, req.WorkflowDefinitions, reg, ordered)...)
	if len(validationErrors) > 0 || len(result.Errors) > 0 {
		result.Valid = false
	}
	if includeImpact {
		for _, role := range req.Roles {
			if role != nil && strings.TrimSpace(role.Name) != "" {
				result.Impact = append(result.Impact, ConfigurationImpactItem{Name: role.Name, Kind: "role", Action: "add", Safe: true})
			}
		}
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

func decodeDraftConfigurationRequest(c *gin.Context) (DraftConfigurationRequest, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, configurationCheckBodyLimit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(c, http.StatusRequestEntityTooLarge, string(contract.CodeValidationFailed), "Configuration body exceeds the 3 MiB limit.", nil)
			return DraftConfigurationRequest{}, false
		}
		badRequestError(c, "validation.invalid_json", "Invalid request body", nil)
		return DraftConfigurationRequest{}, false
	}
	var wire contract.ConfigurationDraftV2
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+err.Error(), nil)
		return DraftConfigurationRequest{}, false
	}
	req := DraftConfigurationRequest{
		ProposalID: wire.ProposalID, DraftConfigVersionID: wire.DraftConfigVersionID,
		ConversationID: wire.ConversationID, BaseConfigVersionID: wire.BaseConfigVersionID,
		CreatedBy:     wire.CreatedBy,
		ScheduledJobs: wire.ScheduledJobs,
	}
	for i, raw := range wire.WorkflowDefinitions {
		definition, err := flow.DecodeWorkflowDefinition(raw)
		if err != nil {
			badRequestError(c, "validation.invalid_json", fmt.Sprintf("Invalid workflow definition %d: %v", i+1, err), nil)
			return DraftConfigurationRequest{}, false
		}
		req.WorkflowDefinitions = append(req.WorkflowDefinitions, definition)
	}
	var decodeErr error
	if req.DocTypes, decodeErr = decodeDraftResources[doctype.DocType]("doctypes", wire.DocTypes); decodeErr != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+decodeErr.Error(), nil)
		return DraftConfigurationRequest{}, false
	}
	if req.Roles, decodeErr = decodeDraftResources[doctype.Role]("roles", wire.Roles); decodeErr != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+decodeErr.Error(), nil)
		return DraftConfigurationRequest{}, false
	}
	if req.Permissions, decodeErr = decodeDraftResources[DraftPermission]("permissions", wire.Permissions); decodeErr != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+decodeErr.Error(), nil)
		return DraftConfigurationRequest{}, false
	}
	if req.Views, decodeErr = decodeDraftResources[doctype.View]("views", wire.Views); decodeErr != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+decodeErr.Error(), nil)
		return DraftConfigurationRequest{}, false
	}
	if req.Scripts, decodeErr = decodeDraftResources[ScriptCandidate]("scripts", wire.Scripts); decodeErr != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+decodeErr.Error(), nil)
		return DraftConfigurationRequest{}, false
	}
	if err := decodeConfigurationCheckViews(body, &req); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid view definition: "+err.Error(), nil)
		return DraftConfigurationRequest{}, false
	}
	return req, true
}

func decodeDraftResources[T any](name string, rawItems []json.RawMessage) ([]*T, error) {
	if rawItems == nil {
		return nil, nil
	}
	items := make([]*T, len(rawItems))
	for index, raw := range rawItems {
		var item *T
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("%s item %d: %w", name, index+1, err)
		}
		items[index] = item
	}
	return items, nil
}

type candidateWorkflowSchemas struct {
	tables map[string]map[string]string
}

func newCandidateWorkflowSchemas(registry *doctype.Registry, proposed []*doctype.DocType) candidateWorkflowSchemas {
	schemas := candidateWorkflowSchemas{tables: make(map[string]map[string]string)}
	add := func(dt *doctype.DocType) {
		if dt == nil || strings.TrimSpace(dt.Name) == "" {
			return
		}
		fields := make(map[string]string, len(dt.Fields))
		for _, field := range dt.Fields {
			fieldType := field.Fieldtype
			if strings.EqualFold(fieldType, "Data") && strings.EqualFold(strings.TrimSpace(field.Options), "Email") {
				fieldType = "Email"
			}
			fields[field.Fieldname] = fieldType
		}
		schemas.tables[dt.Name] = fields
	}
	if registry != nil {
		for _, dt := range registry.All() {
			add(dt)
		}
	}
	for _, dt := range proposed {
		add(dt)
	}
	return schemas
}

func (s candidateWorkflowSchemas) RecordTableExists(table string) bool {
	_, ok := s.tables[table]
	return ok
}

func (s candidateWorkflowSchemas) RecordFieldExists(table, field string) bool {
	_, ok := s.RecordFieldType(table, field)
	return ok
}

func (s candidateWorkflowSchemas) RecordFieldType(table, field string) (string, bool) {
	fields, ok := s.tables[table]
	if !ok {
		return "", false
	}
	fieldType, ok := fields[field]
	return fieldType, ok
}

func validateWorkflowDefinitionCandidates(handler *Handler, c *gin.Context, definitions []contract.WorkflowDefinition, registry *doctype.Registry, proposed []*doctype.DocType) []string {
	if len(definitions) == 0 {
		return nil
	}
	if handler == nil || c == nil {
		return []string{"workflow validation is unavailable"}
	}
	service := handler.runtimeService(c, c.GetString("site_name")).WorkflowDefinitions
	if service == nil {
		return []string{"workflow validation is unavailable for this workspace"}
	}
	issues := make([]string, 0)
	recordSchemas := newCandidateWorkflowSchemas(registry, proposed)
	seen := make(map[contract.DefinitionID]struct{}, len(definitions))
	for i, definition := range definitions {
		if _, exists := seen[definition.ID]; exists {
			issues = append(issues, fmt.Sprintf("workflow_definitions[%d].id: duplicate definition ID %q", i, definition.ID))
			continue
		}
		seen[definition.ID] = struct{}{}
		result := service.ValidateCandidateWithRecordSchemas(definition, recordSchemas)
		for _, issue := range result.Issues {
			issues = append(issues, fmt.Sprintf("workflow_definitions[%d].%s: %s", i, issue.Path, issue.Detail))
		}
	}
	return issues
}

func validateConfigurationCandidateRules(req DraftConfigurationRequest, ordered []*doctype.DocType, registry *doctype.Registry, handler *Handler) ([]string, []string) {
	var messages []string
	var warnings []string
	roleNames := make(map[string]bool, len(req.Roles))
	for _, role := range req.Roles {
		if role == nil || strings.TrimSpace(role.Name) == "" {
			messages = append(messages, "every generated role must have a name")
			continue
		}
		key := normalizeDocTypeName(role.Name)
		if roleNames[key] {
			messages = append(messages, fmt.Sprintf("generated role names conflict: %q", role.Name))
		}
		roleNames[key] = true
	}
	docNames := make(map[string]bool, len(ordered))
	for _, docType := range ordered {
		docNames[normalizeDocTypeName(docType.Name)] = true
	}
	messages = append(messages, validateCandidateDocTypeReferences(ordered, roleNames, registry)...)
	for _, permission := range req.Permissions {
		if permission == nil || strings.TrimSpace(permission.Role) == "" || strings.TrimSpace(permission.Doctype) == "" {
			messages = append(messages, "every permission must name a role and DocType")
			continue
		}
		if permission.Public {
			messages = append(messages, "public permission grants are unsupported; configure an explicit reviewed public-access policy")
		}
		if !docNames[normalizeDocTypeName(permission.Doctype)] && (registry == nil || !registry.Has(permission.Doctype)) {
			messages = append(messages, fmt.Sprintf("permission references unknown DocType %q", permission.Doctype))
		}
		if !roleNames[normalizeDocTypeName(permission.Role)] && permission.Role != doctype.AdminRole {
			warnings = append(warnings, fmt.Sprintf("permission role %q is not in the generated role set", permission.Role))
		}
	}
	messages = append(messages, validateCandidateViews(req.Views, docNames, registry)...)
	if err := validateScriptCandidates(handler, req.Scripts); err != nil {
		messages = append(messages, err.Error())
	}
	return messages, warnings
}

func decodeConfigurationCheckViews(body []byte, req *DraftConfigurationRequest) error {
	var envelope struct {
		Views []json.RawMessage `json:"views"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if envelope.Views == nil {
		return nil
	}
	req.Views = make([]*doctype.View, 0, len(envelope.Views))
	for index, raw := range envelope.Views {
		var view doctype.View
		if err := json.Unmarshal(raw, &view); err != nil {
			return fmt.Errorf("view %d: %w", index+1, err)
		}
		var legacy struct {
			DocType string `json:"doctype"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return fmt.Errorf("view %d: %w", index+1, err)
		}
		if strings.TrimSpace(view.SourceDocType) == "" {
			view.SourceDocType = strings.TrimSpace(legacy.DocType)
		}
		req.Views = append(req.Views, &view)
	}
	return nil
}

func validateCandidateDocTypeReferences(candidates []*doctype.DocType, roleNames map[string]bool, registry *doctype.Registry) []string {
	docs := make(map[string]*doctype.DocType, len(candidates))
	for _, candidate := range candidates {
		if candidate != nil {
			docs[normalizeDocTypeName(candidate.Name)] = candidate
		}
	}
	var messages []string
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		for _, field := range candidate.Fields {
			fieldType := strings.ToLower(strings.TrimSpace(field.Fieldtype))
			if fieldType != "link" && fieldType != "table" {
				continue
			}
			targetName := strings.TrimSpace(field.Options)
			targetKey := normalizeDocTypeName(targetName)
			target := docs[targetKey]
			if target == nil && registry != nil {
				target = registry.Get(targetName)
			}
			// A role and a business DocType may legitimately share a display
			// name (for example, the "Household Member" role and table). Reject
			// role-only targets while allowing links to the real record type.
			if fieldType == "link" && roleNames[targetKey] && target == nil {
				messages = append(messages, fmt.Sprintf("DocType %q field %q cannot link to role %q", candidate.Name, field.Fieldname, targetName))
				continue
			}
			if targetName == "" || target == nil {
				messages = append(messages, fmt.Sprintf("DocType %q field %q references unknown %s target %q", candidate.Name, field.Fieldname, field.Fieldtype, targetName))
				continue
			}
			if fieldType == "table" && !target.IsChildTable {
				messages = append(messages, fmt.Sprintf("DocType %q Table field %q must reference a child DocType, got %q", candidate.Name, field.Fieldname, targetName))
			}
		}
	}
	return messages
}

func validateCandidateViews(views []*doctype.View, candidateNames map[string]bool, registry *doctype.Registry) []string {
	seenNames := map[string]bool{}
	seenRoutes := map[string]bool{}
	var messages []string
	for _, view := range views {
		if view == nil {
			messages = append(messages, "generated views cannot be null")
			continue
		}
		if err := view.Validate(); err != nil {
			messages = append(messages, fmt.Sprintf("view %q failed validation: %v", view.Name, err))
			continue
		}
		nameKey := normalizeDocTypeName(view.Name)
		if seenNames[nameKey] {
			messages = append(messages, fmt.Sprintf("view %q is duplicated", view.Name))
		}
		seenNames[nameKey] = true
		route := strings.TrimSpace(view.Route)
		if !strings.HasPrefix(route, "/") {
			messages = append(messages, fmt.Sprintf("view %q route must start with /", view.Name))
		}
		if seenRoutes[route] {
			messages = append(messages, fmt.Sprintf("view %q route %q is duplicated", view.Name, route))
		}
		seenRoutes[route] = true
		docTypes := make([]string, 0, len(view.Components)+1)
		if docType := strings.TrimSpace(view.SourceDocType); docType != "" {
			docTypes = append(docTypes, docType)
		}
		collectViewComponentDocTypes(view.Components, &docTypes)
		if len(docTypes) == 0 {
			messages = append(messages, fmt.Sprintf("view %q must reference a DocType directly or through a component", view.Name))
			continue
		}
		for _, docType := range docTypes {
			if !candidateNames[normalizeDocTypeName(docType)] && (registry == nil || !registry.Has(docType)) {
				messages = append(messages, fmt.Sprintf("view %q references unknown DocType %q", view.Name, docType))
			}
		}
		messages = append(messages, validateViewTaskDocTypes(view, candidateNames, registry)...)
	}
	return messages
}

func validateViewTaskDocTypes(view *doctype.View, candidateNames map[string]bool, registry *doctype.Registry) []string {
	if view == nil || view.Task == nil {
		return nil
	}
	var messages []string
	for _, name := range append(append([]string(nil), view.Task.Reads...), view.Task.Writes...) {
		name = strings.TrimSpace(name)
		if name == "" || candidateNames[normalizeDocTypeName(name)] || (registry != nil && registry.Has(name)) {
			continue
		}
		messages = append(messages, fmt.Sprintf("view %q task references unknown DocType %q", view.Name, name))
	}
	return messages
}

func collectViewComponentDocTypes(components []doctype.ViewComponent, result *[]string) {
	for _, component := range components {
		if docType := strings.TrimSpace(component.SourceDocType); docType != "" {
			*result = append(*result, docType)
		}
		collectViewComponentDocTypes(component.Components, result)
	}
}
