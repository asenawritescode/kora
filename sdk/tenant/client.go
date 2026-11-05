// Package tenant provides the actor-authenticated public business API client.
// Its credential boundary is separate from sdk/management.
package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
)

const maxResponseBytes = 2 << 20

// TokenSource supplies an ordinary actor or extension credential. Management
// credentials belong to sdk/management and cannot be configured here.
type TokenSource interface {
	Token(context.Context) (string, error)
}

type TokenSourceFunc func(context.Context) (string, error)

func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

type Config struct {
	BaseURL    string
	SiteName   string
	HTTPClient *http.Client
	Tokens     TokenSource
}

type Client struct {
	base   *url.URL
	site   string
	http   *http.Client
	tokens TokenSource
}

// NewClient constructs the public tenant API client. Bearer credentials are
// sent only to the configured site over HTTPS, except on loopback in tests.
func NewClient(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || base == nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("tenant base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if base.Scheme == "http" && !loopbackHost(base.Hostname()) {
		return nil, fmt.Errorf("tenant bearer credentials require HTTPS except on loopback")
	}
	site := strings.TrimSpace(cfg.SiteName)
	if site == "" || len(site) > 253 || strings.ContainsAny(site, "/\\?#\r\n\x00") {
		return nil, fmt.Errorf("tenant site name is invalid")
	}
	if cfg.Tokens == nil {
		return nil, fmt.Errorf("tenant client requires an actor token source")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	httpCopy := *cfg.HTTPClient
	httpCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	base.Path = strings.TrimRight(base.Path, "/")
	return &Client{base: base, site: site, http: &httpCopy, tokens: cfg.Tokens}, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && address.IsLoopback()
}

// WithScope returns a new immutable client bound to one verified business
// scope. Concurrent requests using different scopes cannot overwrite each
// other's entity context.
func (c *Client) WithScope(scope contract.BusinessScope) (*ScopedClient, error) {
	if c == nil || c.base == nil || scope.Validate() != nil {
		return nil, fmt.Errorf("invalid tenant client or business scope")
	}
	return &ScopedClient{client: c, scope: scope}, nil
}

type ScopedClient struct {
	client *Client
	scope  contract.BusinessScope
}

func (c *ScopedClient) Scope() contract.BusinessScope { return c.scope }

func (c *ScopedClient) Catalog(ctx context.Context) (contract.ApplicationCatalog, error) {
	var result contract.ApplicationCatalog
	err := c.do(ctx, http.MethodGet, "/api/v2/modules/catalog", nil, http.StatusOK, &result)
	if err == nil {
		err = result.Validate()
		if err == nil && result.BusinessScope != c.scope {
			err = fmt.Errorf("application catalog scope does not match scoped client")
		}
	}
	return result, err
}

func (c *ScopedClient) PublishedQuery(ctx context.Context, catalog contract.ApplicationCatalog, owner contract.ModuleID, name string, request contract.PublishedQueryRequest) (contract.QueryResult, error) {
	var result contract.QueryResult
	if err := c.validateCatalog(catalog); err != nil {
		return result, err
	}
	if err := request.Validate(); err != nil {
		return result, fmt.Errorf("validate published query request: %w", err)
	}
	var selected *contract.ApplicationCatalogQuery
	for i := range catalog.PublishedQueries {
		query := &catalog.PublishedQueries[i]
		if query.OwnerModuleID == owner && query.Name == name {
			selected = query
			break
		}
	}
	if selected == nil {
		return result, fmt.Errorf("published query is not present in actor catalog")
	}
	err := c.do(ctx, http.MethodPost, selected.Route, request, http.StatusOK, &result)
	if err == nil {
		err = validateQueryResult(result)
	}
	return result, err
}

func (c *ScopedClient) Projection(ctx context.Context, catalog contract.ApplicationCatalog, owner contract.ModuleID, id string) (contract.QueryResult, error) {
	var result contract.QueryResult
	if err := c.validateCatalog(catalog); err != nil {
		return result, err
	}
	var selected *contract.ApplicationCatalogProjection
	for i := range catalog.PublishedProjections {
		projection := &catalog.PublishedProjections[i]
		if projection.OwnerModuleID == owner && projection.ID == id {
			selected = projection
			break
		}
	}
	if selected == nil {
		return result, fmt.Errorf("published projection is not present in actor catalog")
	}
	err := c.do(ctx, http.MethodGet, selected.Route, nil, http.StatusOK, &result)
	if err == nil {
		err = validateQueryResult(result)
	}
	return result, err
}

func (c *ScopedClient) ExecutePublishedOperation(ctx context.Context, catalog contract.ApplicationCatalog, operationRef string, command contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	var result contract.OperationReceipt
	if err := c.validateCatalog(catalog); err != nil {
		return result, err
	}
	if err := command.Validate(); err != nil {
		return result, fmt.Errorf("validate published operation request: %w", err)
	}
	var selected *contract.ApplicationCatalogOperation
	for i := range catalog.PublishedOperations {
		operation := &catalog.PublishedOperations[i]
		if operation.OperationRef == operationRef {
			selected = operation
			break
		}
	}
	if selected == nil || selected.Availability != contract.OperationAvailable || command.InputSchema != selected.InputSchema {
		return result, fmt.Errorf("published operation is unavailable or its input schema does not match the actor catalog")
	}
	route := "/api/v2/modules/operations/" + url.PathEscape(operationRef) + "/execute"
	err := c.doWithHeaders(ctx, http.MethodPost, route, command, http.StatusOK, &result, map[string]string{
		"Idempotency-Key": string(command.Meta.IdempotencyKey),
	})
	if err == nil {
		err = validateReceipt(result, c.scope, operationRef)
	}
	return result, err
}

// Execution returns a previously issued receipt and never dispatches work.
func (c *ScopedClient) Execution(ctx context.Context, catalog contract.ApplicationCatalog, executionID contract.OperationID) (contract.OperationReceipt, error) {
	var result contract.OperationReceipt
	if err := c.validateCatalog(catalog); err != nil {
		return result, err
	}
	if _, err := contract.ParseOperationID(string(executionID)); err != nil {
		return result, fmt.Errorf("validate execution ID: %w", err)
	}
	route := "/api/v2/module-executions/" + url.PathEscape(string(executionID))
	err := c.do(ctx, http.MethodGet, route, nil, http.StatusOK, &result)
	if err == nil {
		err = validateReceipt(result, c.scope, "")
	}
	return result, err
}

// LookupExecution recovers a receipt by the original operation and
// idempotency key. It does not resubmit or redispatch the operation.
func (c *ScopedClient) LookupExecution(ctx context.Context, catalog contract.ApplicationCatalog, request contract.LookupPublishedOperation) (contract.OperationReceipt, error) {
	var result contract.OperationReceipt
	if err := c.validateCatalog(catalog); err != nil {
		return result, err
	}
	if err := request.Validate(); err != nil {
		return result, fmt.Errorf("validate execution lookup: %w", err)
	}
	err := c.do(ctx, http.MethodPost, "/api/v2/module-executions/lookup", request, http.StatusOK, &result)
	if err == nil {
		err = validateReceipt(result, c.scope, request.OperationRef)
	}
	return result, err
}

// StartWorkflow starts one durable workflow intent under this client's scope.
// A timeout does not authorize callers to mint a new idempotency key.
func (c *ScopedClient) StartWorkflow(ctx context.Context, command contract.StartWorkflow) (contract.WorkflowInstance, error) {
	var result contract.WorkflowInstance
	if command.BusinessScope.TenantID == "" {
		command.BusinessScope = c.scope
	}
	if command.BusinessScope != c.scope || command.Meta.IdempotencyKey == "" || command.DefinitionID == "" || command.DefinitionVersion == 0 || command.Subject.Kind == "" || command.Subject.ID == "" || command.Subject.Revision == "" {
		return result, fmt.Errorf("workflow start identity or scope is invalid")
	}
	if err := c.do(ctx, http.MethodPost, "/api/v2/workflow-instances", command, http.StatusCreated, &result); err == nil {
		err = validateWorkflowInstance(result, c.scope)
		return result, err
	} else {
		return result, err
	}
}

func (c *ScopedClient) Workflow(ctx context.Context, instanceID contract.InstanceID) (contract.WorkflowInstance, error) {
	var result contract.WorkflowInstance
	if _, err := contract.ParseInstanceID(string(instanceID)); err != nil {
		return result, fmt.Errorf("validate workflow instance ID: %w", err)
	}
	err := c.do(ctx, http.MethodGet, "/api/v2/workflow-instances/"+url.PathEscape(string(instanceID)), nil, http.StatusOK, &result)
	if err == nil {
		err = validateWorkflowInstance(result, c.scope)
	}
	return result, err
}

func (c *ScopedClient) WorkflowInstances(ctx context.Context) ([]contract.WorkflowInstance, error) {
	var result []contract.WorkflowInstance
	err := c.do(ctx, http.MethodGet, "/api/v2/workflow-instances", nil, http.StatusOK, &result)
	if err == nil {
		if len(result) > 50 {
			return nil, fmt.Errorf("workflow instance list exceeds 50 items")
		}
		for _, instance := range result {
			if validateWorkflowInstance(instance, c.scope) != nil {
				return nil, fmt.Errorf("workflow instance list contains invalid scope or identity")
			}
		}
	}
	return result, err
}

func (c *ScopedClient) WorkflowHistory(ctx context.Context, instanceID contract.InstanceID) ([]contract.WorkflowHistoryEntry, error) {
	var result []contract.WorkflowHistoryEntry
	if _, err := contract.ParseInstanceID(string(instanceID)); err != nil {
		return nil, fmt.Errorf("validate workflow instance ID: %w", err)
	}
	err := c.do(ctx, http.MethodGet, "/api/v2/workflow-instances/"+url.PathEscape(string(instanceID))+"/history", nil, http.StatusOK, &result)
	if err == nil {
		if len(result) > 100 {
			return nil, fmt.Errorf("workflow history exceeds 100 entries")
		}
		for _, entry := range result {
			if entry.Sequence == 0 || entry.Kind == "" || entry.Status == "" || entry.StepID == "" || entry.CreatedAt.IsZero() || len(entry.Detail) > 8192 {
				return nil, fmt.Errorf("workflow history contains an invalid entry")
			}
		}
	}
	return result, err
}

func (c *ScopedClient) CancelWorkflow(ctx context.Context, instanceID contract.InstanceID, command contract.CancelWorkflow) (contract.WorkflowInstance, error) {
	var result contract.WorkflowInstance
	if _, err := contract.ParseInstanceID(string(instanceID)); err != nil {
		return result, fmt.Errorf("validate workflow instance ID: %w", err)
	}
	if command.Meta.IdempotencyKey == "" || strings.TrimSpace(command.Reason) == "" || len(command.Reason) > 8192 {
		return result, fmt.Errorf("workflow cancellation identity or reason is invalid")
	}
	err := c.do(ctx, http.MethodPost, "/api/v2/workflow-instances/"+url.PathEscape(string(instanceID))+"/cancel", command, http.StatusOK, &result)
	if err == nil {
		err = validateWorkflowInstance(result, c.scope)
	}
	return result, err
}

func (c *ScopedClient) ReconcileWorkflow(ctx context.Context, instanceID contract.InstanceID, command contract.ReconcileWorkflow) (contract.WorkflowInstance, error) {
	var result contract.WorkflowInstance
	if _, err := contract.ParseInstanceID(string(instanceID)); err != nil {
		return result, fmt.Errorf("validate workflow instance ID: %w", err)
	}
	if command.Meta.IdempotencyKey == "" || command.StepID == "" || command.Reason == "" || len(command.Reason) > 8192 || len(command.EvidenceRef) > 512 || (command.Outcome != contract.ReconcileSucceeded && command.Outcome != contract.ReconcileNotApplied && command.Outcome != contract.ReconcileAbandon) {
		return result, fmt.Errorf("workflow reconciliation identity or evidence is invalid")
	}
	err := c.do(ctx, http.MethodPost, "/api/v2/workflow-instances/"+url.PathEscape(string(instanceID))+"/reconcile", command, http.StatusOK, &result)
	if err == nil {
		err = validateWorkflowInstance(result, c.scope)
	}
	return result, err
}

func (c *ScopedClient) BusinessReconciliationSummary(ctx context.Context) (contract.BusinessReconciliationSummary, error) {
	var result contract.BusinessReconciliationSummary
	err := c.do(ctx, http.MethodGet, "/api/v1/modules/reconciliation/findings", nil, http.StatusOK, &result)
	if err == nil && result.Scope != c.scope {
		err = fmt.Errorf("business reconciliation summary scope does not match scoped client")
	}
	return result, err
}

func (c *ScopedClient) RunBusinessReconciliation(ctx context.Context, request contract.BusinessReconciliationRunRequest) (contract.BusinessRun, error) {
	var result contract.BusinessRun
	if err := request.Validate(); err != nil {
		return result, err
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/modules/reconciliation/runs", request, http.StatusOK, &result)
	if err == nil && (result.Scope != c.scope || result.OwnerModuleID != request.OwnerModuleID || result.RuleID != request.RuleID || result.ID != request.RunID) {
		err = fmt.Errorf("business reconciliation run identity does not match request")
	}
	return result, err
}

func (c *ScopedClient) BusinessReconciliationRun(ctx context.Context, owner contract.ModuleID, ruleID, runID string) (contract.BusinessRun, error) {
	var result contract.BusinessRun
	if owner == "" || ruleID == "" || runID == "" {
		return result, fmt.Errorf("business reconciliation run identity is required")
	}
	route := "/api/v1/modules/reconciliation/runs/" + url.PathEscape(string(owner)) + "/" + url.PathEscape(ruleID) + "/" + url.PathEscape(runID)
	err := c.do(ctx, http.MethodGet, route, nil, http.StatusOK, &result)
	if err == nil && (result.Scope != c.scope || result.OwnerModuleID != owner || result.RuleID != ruleID || result.ID != runID) {
		err = fmt.Errorf("business reconciliation run identity does not match request")
	}
	return result, err
}

func (c *ScopedClient) WaiveBusinessFinding(ctx context.Context, owner contract.ModuleID, ruleID, runID string, request contract.BusinessFindingDecision) (contract.BusinessFindingActionResult, error) {
	var result contract.BusinessFindingActionResult
	if err := request.Validate(true); err != nil {
		return result, err
	}
	route := "/api/v1/modules/reconciliation/runs/" + url.PathEscape(string(owner)) + "/" + url.PathEscape(ruleID) + "/" + url.PathEscape(runID) + "/waive"
	err := c.do(ctx, http.MethodPost, route, request, http.StatusOK, &result)
	if err == nil && (result.FindingID != request.FindingID || result.Action != contract.BusinessFindingActionWaived) {
		err = fmt.Errorf("business finding waiver response identity is invalid")
	}
	return result, err
}

func (c *ScopedClient) RepairBusinessFinding(ctx context.Context, owner contract.ModuleID, ruleID, runID string, request contract.BusinessFindingDecision) (contract.BusinessFindingActionResult, error) {
	var result contract.BusinessFindingActionResult
	if err := request.Validate(false); err != nil {
		return result, err
	}
	route := "/api/v1/modules/reconciliation/runs/" + url.PathEscape(string(owner)) + "/" + url.PathEscape(ruleID) + "/" + url.PathEscape(runID) + "/repair"
	err := c.do(ctx, http.MethodPost, route, request, http.StatusOK, &result)
	if err == nil && (result.FindingID != request.FindingID || result.Action != contract.BusinessFindingActionRepaired) {
		err = fmt.Errorf("business finding repair response identity is invalid")
	}
	return result, err
}

func (c *ScopedClient) validateCatalog(catalog contract.ApplicationCatalog) error {
	if err := catalog.Validate(); err != nil {
		return fmt.Errorf("validate actor catalog: %w", err)
	}
	if catalog.BusinessScope != c.scope {
		return fmt.Errorf("application catalog scope does not match scoped client")
	}
	return nil
}

func (c *ScopedClient) do(ctx context.Context, method, endpoint string, input any, expected int, output any) error {
	return c.doWithHeaders(ctx, method, endpoint, input, expected, output, nil)
}

func (c *ScopedClient) doWithHeaders(ctx context.Context, method, endpoint string, input any, expected int, output any, headers map[string]string) error {
	if c == nil || c.client == nil || c.client.base == nil {
		return fmt.Errorf("tenant scoped client is nil")
	}
	relative, err := url.Parse(endpoint)
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.RawQuery != "" || relative.Fragment != "" || !strings.HasPrefix(relative.Path, "/api/") || strings.Contains(relative.Path, "..") {
		return fmt.Errorf("invalid tenant API endpoint")
	}
	token, err := c.client.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("get tenant actor token: %w", err)
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("tenant actor token is empty or invalid")
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode tenant request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpointURL := *c.client.base
	escapedPath := strings.TrimRight(c.client.base.EscapedPath(), "/") + "/s/" + url.PathEscape(c.client.site) + relative.EscapedPath()
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return fmt.Errorf("encode tenant API path: %w", err)
	}
	endpointURL.Path = decodedPath
	endpointURL.RawPath = escapedPath
	request, err := http.NewRequestWithContext(ctx, method, endpointURL.String(), body)
	if err != nil {
		return fmt.Errorf("create tenant request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	for name, value := range headers {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("tenant request header is invalid")
		}
		request.Header.Set(name, value)
	}
	if c.scope.Kind == contract.ScopeKindLegalEntity {
		request.Header.Set("X-Kora-Legal-Entity-ID", c.scope.LegalEntityID)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.http.Do(request)
	if err != nil {
		return fmt.Errorf("send tenant request: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read tenant response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("tenant response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode != expected {
		return decodeRemoteError(response.StatusCode, data)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope struct {
		Data json.RawMessage `json:"data"`
		Meta json.RawMessage `json:"meta,omitempty"`
	}
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("decode tenant response envelope: missing or malformed data")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("decode tenant response envelope: trailing data")
	}
	dataDecoder := json.NewDecoder(bytes.NewReader(envelope.Data))
	dataDecoder.DisallowUnknownFields()
	if err := dataDecoder.Decode(output); err != nil {
		return fmt.Errorf("decode tenant response data: %w", err)
	}
	if err := dataDecoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("decode tenant response data: trailing data")
	}
	return nil
}

type RemoteError struct {
	Status int
	Body   *contract.Error
}

func (e *RemoteError) Error() string {
	if e == nil || e.Body == nil {
		return "Engine tenant request failed"
	}
	return fmt.Sprintf("Engine tenant request failed (%d, %s): %s", e.Status, e.Body.Type, e.Body.Message)
}

func decodeRemoteError(status int, data []byte) error {
	var envelope struct {
		Error json.RawMessage `json:"error"`
		Meta  json.RawMessage `json:"meta,omitempty"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Error) == 0 {
		return &RemoteError{Status: status, Body: &contract.Error{Type: contract.CodeInternal, Message: http.StatusText(status)}}
	}
	var body contract.Error
	if err := json.Unmarshal(envelope.Error, &body); err == nil && body.Type != "" {
		return &RemoteError{Status: status, Body: &body}
	}
	return &RemoteError{Status: status, Body: &contract.Error{Type: contract.CodeInternal, Message: http.StatusText(status)}}
}

func validateQueryResult(result contract.QueryResult) error {
	if len(result.Data) == 0 || len(result.Data) > 1<<20 || !json.Valid(result.Data) || result.AsOf.IsZero() || len(result.NextCursor) > 1024 || len(result.SnapshotToken) > 256 || len(result.SourceCursor) > 256 || len(result.SourceRevision) > 128 {
		return fmt.Errorf("published query result is invalid or exceeds bounds")
	}
	switch result.Freshness {
	case contract.QueryFresh, contract.QueryStale, contract.QueryUnknown, contract.QueryIncomplete:
	default:
		return fmt.Errorf("published query result freshness is invalid")
	}
	return nil
}

func validateReceipt(receipt contract.OperationReceipt, scope contract.BusinessScope, operationRef string) error {
	if err := receipt.Validate(); err != nil {
		return fmt.Errorf("validate operation receipt: %w", err)
	}
	if _, err := contract.ParseOperationID(string(receipt.ExecutionID)); err != nil {
		return fmt.Errorf("validate operation receipt execution ID: %w", err)
	}
	if receipt.BusinessScope != scope || operationRef != "" && receipt.OperationRef != operationRef {
		return fmt.Errorf("operation receipt identity does not match requested scope and operation")
	}
	return nil
}

func validateWorkflowInstance(instance contract.WorkflowInstance, scope contract.BusinessScope) error {
	if _, err := contract.ParseInstanceID(string(instance.ID)); err != nil {
		return fmt.Errorf("workflow instance ID is invalid")
	}
	if instance.DefinitionID == "" || instance.DefinitionVersion == 0 || instance.Subject.ID == "" || instance.Subject.Kind == "" || instance.Revision == "" || instance.CreatedAt.IsZero() || instance.UpdatedAt.IsZero() {
		return fmt.Errorf("workflow instance identity is incomplete")
	}
	if instance.BusinessScope.TenantID != "" && instance.BusinessScope != scope {
		return fmt.Errorf("workflow instance scope does not match scoped client")
	}
	switch instance.Status {
	case contract.InstanceReady, contract.InstanceRunning, contract.InstanceWaiting, contract.InstanceRetrying, contract.InstanceCompleted, contract.InstanceCancelled, contract.InstanceFailed, contract.InstanceUncertain:
	default:
		return fmt.Errorf("workflow instance status is invalid")
	}
	return nil
}
