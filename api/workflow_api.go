package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/workflow"
	"github.com/gin-gonic/gin"
)

const workflowBodyLimit = 1 << 20

// RegisterWorkflowRoutes exposes only durable process orchestration APIs. The
// caller must attach tenant routing, session/CSRF, compression and admission
// middleware to the group.
func RegisterWorkflowRoutes(group *gin.RouterGroup, services *SiteRuntimeServices) {
	h := &workflowHandler{services: services}
	group.POST("/workflow-definitions/validate", h.validateDefinition)
	group.POST("/workflow-definitions/preview", h.previewDefinition)
	group.GET("/workflow-previews", h.listPreviews)
	group.GET("/workflow-capabilities/:table", h.capabilities)
	group.POST("/workflow-definitions", h.publishDefinition)
	group.GET("/workflow-definitions", h.listDefinitions)
	group.GET("/workflow-definitions/:id/:version", h.getDefinition)
	group.POST("/workflow-instances", h.start)
	group.GET("/workflow-instances", h.listInstances)
	group.GET("/workflow-instances/:id", h.get)
	group.GET("/workflow-instances/:id/history", h.history)
	group.POST("/workflow-instances/:id/reconcile", h.reconcile)
	group.POST("/workflow-instances/:id/cancel", h.cancel)
}

type WorkflowTableCapability struct {
	Table            string                         `json:"table"`
	StateField       string                         `json:"state_field,omitempty"`
	InitialState     string                         `json:"initial_state,omitempty"`
	AdminRole        string                         `json:"admin_role"`
	Triggers         []string                       `json:"triggers"`
	Fields           []WorkflowFieldCapability      `json:"fields"`
	Actions          []WorkflowActionCapability     `json:"actions"`
	ApprovalPolicies []string                       `json:"approval_policies"`
	Transitions      []WorkflowTransitionCapability `json:"transitions"`
	Blockers         []string                       `json:"blockers,omitempty"`
}

type WorkflowTransitionCapability struct {
	Action         string   `json:"action"`
	From           string   `json:"from"`
	To             string   `json:"to"`
	Allowed        string   `json:"allowed,omitempty"`
	RequiredFields []string `json:"required_fields,omitempty"`
}

type WorkflowFieldCapability struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Options string `json:"options,omitempty"`
}

type WorkflowActionCapability struct {
	Name        string `json:"name"`
	Version     uint32 `json:"version"`
	Class       string `json:"class"`
	InputSchema string `json:"input_schema"`
}

func (h *workflowHandler) capabilities(c *gin.Context) {
	execution, definitions, _, ok := h.identity(c)
	if !ok {
		return
	}
	if _, err := definitions.ListLatest(c.Request.Context(), execution); err != nil {
		writeRequestError(c, err)
		return
	}
	value, found := c.Get("site_registry")
	registry, valid := value.(*doctype.Registry)
	if !found || !valid || registry == nil {
		writeError(c, http.StatusServiceUnavailable, string(contract.CodeDependencyUnavailable), "Table schema is unavailable.", nil)
		return
	}
	table := c.Param("table")
	dt := registry.Get(table)
	if dt == nil || dt.Name != table || dt.IsChildTable {
		writeError(c, http.StatusNotFound, string(contract.CodeNotFound), "Table was not found.", nil)
		return
	}
	if _, err := policy.Decide(execution, registry, table, "read"); err != nil {
		writeError(c, http.StatusForbidden, string(contract.CodePermissionDenied), "You do not have access to this table.", nil)
		return
	}
	services := RuntimeServiceForContext(c, h.services, c.GetString("site_name"))
	response := WorkflowTableCapability{Table: table, AdminRole: doctype.AdminRole, Triggers: []string{"record.created", "record.updated"}, Fields: make([]WorkflowFieldCapability, 0), Actions: make([]WorkflowActionCapability, 0), ApprovalPolicies: append([]string(nil), services.WorkflowApprovalPoliciesByTable[table]...), Transitions: make([]WorkflowTransitionCapability, 0)}
	for _, field := range dt.Fields {
		if field.Hidden || field.Fieldname == "" || field.Fieldtype == "Section Break" || field.Fieldtype == "Column Break" {
			continue
		}
		response.Fields = append(response.Fields, WorkflowFieldCapability{Name: field.Fieldname, Label: field.Label, Type: field.Fieldtype, Options: field.Options})
	}
	for _, action := range definitions.AvailableActions() {
		response.Actions = append(response.Actions, WorkflowActionCapability{Name: action.Key.Name, Version: action.Key.Version, Class: string(action.Class), InputSchema: action.InputSchema.ID})
	}
	var statusPolicy *workflow.StatusPolicy
	found = false
	if reader, ok := services.WorkflowStatusPolicies.(workflow.StatusPolicyReader); ok {
		var policyErr error
		statusPolicy, found, policyErr = reader.GetStatusPolicy(c.Request.Context(), execution.Tenant(), table)
		if policyErr != nil {
			writeRequestError(c, policyErr)
			return
		}
	}
	if found && statusPolicy != nil {
		response.StateField = statusPolicy.StateField
		response.InitialState = statusPolicy.InitialState
		for _, transition := range statusPolicy.Transitions {
			response.Transitions = append(response.Transitions, WorkflowTransitionCapability{Action: transition.Action, From: transition.From, To: transition.To, Allowed: strings.Join(transition.AllowedRoles, ", "), RequiredFields: append([]string(nil), transition.RequireFields...)})
		}
	}
	if len(response.Transitions) == 0 {
		response.Blockers = append(response.Blockers, "This table has no published status transitions yet.")
	}
	c.JSON(http.StatusOK, Response{Data: response})
}

func (h *workflowHandler) listDefinitions(c *gin.Context) {
	execution, definitions, _, ok := h.identity(c)
	if !ok {
		return
	}
	refs, err := definitions.ListLatest(c.Request.Context(), execution)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: refs})
}

func (h *workflowHandler) getDefinition(c *gin.Context) {
	execution, definitions, _, ok := h.identity(c)
	if !ok {
		return
	}
	id, err := contract.ParseDefinitionID(c.Param("id"))
	if err != nil {
		badRequestError(c, string(contract.CodeValidationFailed), "workflow definition ID is invalid", nil)
		return
	}
	version64, err := strconv.ParseUint(c.Param("version"), 10, 32)
	if err != nil || version64 == 0 {
		badRequestError(c, string(contract.CodeValidationFailed), "workflow definition version must be positive", nil)
		return
	}
	definition, ref, err := definitions.Get(c.Request.Context(), execution, id, uint32(version64))
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"definition": definition, "reference": ref}})
}

type workflowHandler struct{ services *SiteRuntimeServices }

func (h *workflowHandler) identity(c *gin.Context) (policy.Execution, *workflow.Service, *workflow.InstanceService, bool) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, string(contract.CodeUnauthenticated), "A verified tenant session is required.", nil)
		return policy.Execution{}, nil, nil, false
	}
	services := RuntimeServiceForContext(c, h.services, c.GetString("site_name"))
	if services.WorkflowDefinitions == nil || services.WorkflowInstances == nil {
		writeError(c, http.StatusServiceUnavailable, string(contract.CodeDependencyUnavailable), "Durable workflow orchestration is unavailable for this tenant.", nil)
		return policy.Execution{}, nil, nil, false
	}
	return execution, services.WorkflowDefinitions, services.WorkflowInstances, true
}

func (h *workflowHandler) validateDefinition(c *gin.Context) {
	execution, definitions, _, ok := h.identity(c)
	if !ok {
		return
	}
	var definition contract.WorkflowDefinition
	if !decodeWorkflowBody(c, &definition) {
		return
	}
	result, err := definitions.Validate(c.Request.Context(), execution, definition)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	issues := approvalPolicyIssues(definition, RuntimeServiceForContext(c, h.services, c.GetString("site_name")).WorkflowApprovalPoliciesByTable)
	result.Issues = append(result.Issues, issues...)
	result.Valid = result.Valid && len(issues) == 0
	c.JSON(http.StatusOK, Response{Data: result})
}

func approvalPolicyIssues(definition contract.WorkflowDefinition, policiesByTable map[string][]string) []contract.ValidationIssue {
	if definition.Trigger == nil || len(policiesByTable) == 0 {
		return nil
	}
	available := make(map[string]struct{}, len(policiesByTable[definition.Trigger.Table]))
	for _, policyID := range policiesByTable[definition.Trigger.Table] {
		available[policyID] = struct{}{}
	}
	issues := make([]contract.ValidationIssue, 0)
	for i, step := range definition.Steps {
		if step.Human == nil || step.Human.Request.PolicyID == "" {
			continue
		}
		if _, found := available[step.Human.Request.PolicyID]; !found {
			issues = append(issues, contract.ValidationIssue{Path: fmt.Sprintf("steps[%d].human.request.policy_id", i), Code: "unsupported_approval_policy", Detail: "approval policy is not enabled for the selected table"})
		}
	}
	return issues
}

type workflowPreviewRequest struct {
	Definition contract.WorkflowDefinition `json:"definition"`
	Record     json.RawMessage             `json:"record"`
}

func (h *workflowHandler) previewDefinition(c *gin.Context) {
	execution, definitions, _, ok := h.identity(c)
	if !ok {
		return
	}
	var request workflowPreviewRequest
	if !decodeWorkflowBody(c, &request) {
		return
	}
	if issues := approvalPolicyIssues(request.Definition, RuntimeServiceForContext(c, h.services, c.GetString("site_name")).WorkflowApprovalPoliciesByTable); len(issues) > 0 {
		badRequestError(c, string(contract.CodeValidationFailed), "workflow approval policy is not enabled for the selected table", gin.H{"issues": issues})
		return
	}
	run, err := definitions.Preview(c.Request.Context(), execution, request.Definition, request.Record)
	if err != nil && run.ID == "" {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: run})
}

func (h *workflowHandler) listPreviews(c *gin.Context) {
	execution, definitions, _, ok := h.identity(c)
	if !ok {
		return
	}
	limit := uint64(25)
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || parsed == 0 || parsed > 100 {
			badRequestError(c, string(contract.CodeValidationFailed), "preview history limit must be between 1 and 100", nil)
			return
		}
		limit = parsed
	}
	runs, err := definitions.PreviewHistory(c.Request.Context(), execution, uint16(limit))
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: runs})
}

func (h *workflowHandler) publishDefinition(c *gin.Context) {
	execution, definitions, _, ok := h.identity(c)
	if !ok {
		return
	}
	var definition contract.WorkflowDefinition
	if !decodeWorkflowBody(c, &definition) {
		return
	}
	if issues := approvalPolicyIssues(definition, RuntimeServiceForContext(c, h.services, c.GetString("site_name")).WorkflowApprovalPoliciesByTable); len(issues) > 0 {
		badRequestError(c, string(contract.CodeValidationFailed), "workflow approval policy is not enabled for the selected table", gin.H{"issues": issues})
		return
	}
	key, err := contract.ParseIdempotencyKey(c.GetHeader("Idempotency-Key"))
	if err != nil {
		badRequestError(c, string(contract.CodeValidationFailed), "a valid Idempotency-Key header is required", nil)
		return
	}
	ref, err := definitions.Publish(c.Request.Context(), execution, definition, key)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusCreated, Response{Data: ref})
}

func (h *workflowHandler) start(c *gin.Context) {
	execution, _, instances, ok := h.identity(c)
	if !ok {
		return
	}
	var command contract.StartWorkflow
	if !decodeWorkflowBody(c, &command) {
		return
	}
	if !requestIdempotencyKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	instance, err := instances.Start(c.Request.Context(), execution, command)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusCreated, Response{Data: instance})
}

func (h *workflowHandler) get(c *gin.Context) {
	execution, _, instances, ok := h.identity(c)
	if !ok {
		return
	}
	id, valid := workflowInstanceID(c)
	if !valid {
		return
	}
	instance, err := instances.Get(c.Request.Context(), execution, id)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: instance})
}

func (h *workflowHandler) listInstances(c *gin.Context) {
	execution, _, instances, ok := h.identity(c)
	if !ok {
		return
	}
	result, err := instances.Recent(c.Request.Context(), execution, 50)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *workflowHandler) history(c *gin.Context) {
	execution, _, instances, ok := h.identity(c)
	if !ok {
		return
	}
	id, valid := workflowInstanceID(c)
	if !valid {
		return
	}
	entries, err := instances.History(c.Request.Context(), execution, id, 100)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: entries})
}

func (h *workflowHandler) reconcile(c *gin.Context) {
	execution, _, instances, ok := h.identity(c)
	if !ok {
		return
	}
	id, valid := workflowInstanceID(c)
	if !valid {
		return
	}
	var command contract.ReconcileWorkflow
	if !decodeWorkflowBody(c, &command) {
		return
	}
	if !requestIdempotencyKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	instance, err := instances.Reconcile(c.Request.Context(), execution, id, command)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: instance})
}

func (h *workflowHandler) cancel(c *gin.Context) {
	execution, _, instances, ok := h.identity(c)
	if !ok {
		return
	}
	id, valid := workflowInstanceID(c)
	if !valid {
		return
	}
	var command contract.CancelWorkflow
	if !decodeWorkflowBody(c, &command) {
		return
	}
	if !requestIdempotencyKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	instance, err := instances.Cancel(c.Request.Context(), execution, id, command)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: instance})
}

func workflowInstanceID(c *gin.Context) (contract.InstanceID, bool) {
	id, err := contract.ParseInstanceID(c.Param("id"))
	if err != nil {
		badRequestError(c, string(contract.CodeValidationFailed), "workflow instance ID is invalid", nil)
		return "", false
	}
	return id, true
}

func decodeWorkflowBody(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, workflowBodyLimit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		status, message := http.StatusBadRequest, "Request body is invalid."
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status, message = http.StatusRequestEntityTooLarge, "Request body exceeds the 1 MiB workflow limit."
		}
		writeError(c, status, string(contract.CodeValidationFailed), message, nil)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(c, http.StatusBadRequest, string(contract.CodeValidationFailed), "Request body must contain exactly one JSON value.", nil)
		return false
	}
	return true
}
