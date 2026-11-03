// Package management provides the typed Engine tenant-management HTTP client.
package management

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
)

const maxResponseBytes = 1 << 20

// Client is the supported typed management API used by Cloud and operators.
type Client interface {
	ValidatePack(context.Context, contract.PackManifest, []contract.PackFile) (contract.PackValidation, error)
	Provision(context.Context, contract.ProvisionTenant) (contract.ManagedOperation, error)
	Inspect(context.Context, contract.TenantID) (contract.TenantDescriptorV2, error)
	InspectAlias(context.Context, string) (contract.TenantDescriptorV2, error)
	ApplyPackage(context.Context, contract.ApplyPackage) (contract.ManagedOperation, error)
	SetState(context.Context, contract.TenantID, contract.SetTenantState) (contract.ManagedOperation, error)
	ReconcileTenantStateOperation(context.Context, contract.OperationID, contract.ReconcileTenantStateOperation) (contract.ManagedOperation, error)
	ReconcileTenantProvisionOperation(context.Context, contract.OperationID, contract.ReconcileTenantProvisionOperation) (contract.ManagedOperation, error)
	ReconcileTenantPackageOperation(context.Context, contract.OperationID, contract.ReconcileTenantPackageOperation) (contract.ManagedOperation, error)
	ReconcileTenantDeleteOperation(context.Context, contract.OperationID, contract.ReconcileTenantDeleteOperation) (contract.ManagedOperation, error)
	Delete(context.Context, contract.TenantID, contract.DeleteTenant) (contract.ManagedOperation, error)
	Operation(context.Context, contract.OperationID) (contract.ManagedOperation, error)
	Capabilities(context.Context) (contract.ManagementCapabilitiesV2, error)
	InviteTenantAdministrator(context.Context, contract.TenantID, contract.RequestTenantInvitation) (contract.ManagedOperation, error)
	ReconcileTenantInvitationOperation(context.Context, contract.OperationID, contract.ReconcileTenantInvitationOperation) (contract.ManagedOperation, error)
	ResolveDirectoryEntry(context.Context, contract.TenantID) (contract.SiteDirectoryEntryV2, error)
	ResolveDirectoryAlias(context.Context, string) (contract.SiteDirectoryEntryV2, error)
	PlanModuleChange(context.Context, contract.TenantID, contract.PlanModuleChange) (contract.ModuleChangePlan, error)
	ApplyModuleChange(context.Context, contract.TenantID, contract.ApplyModuleChange) (contract.ManagedOperation, error)
	InspectModules(context.Context, contract.TenantID) (contract.ModuleInspection, error)
	InspectExecution(context.Context, contract.TenantID, string) (contract.ExecutionInspection, error)
}

// TokenSource supplies a scoped service credential for each request. It must
// not return managed-channel tenant credentials.
type TokenSource interface {
	Token(context.Context) (string, error)
}

type TokenSourceFunc func(context.Context) (string, error)

func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

type Config struct {
	BaseURL    string
	HTTPClient *http.Client
	Tokens     TokenSource
}

type httpClient struct {
	base   *url.URL
	http   *http.Client
	tokens TokenSource
}

func NewClient(cfg Config) (Client, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || base == nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("management base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if base.Scheme == "http" && !loopbackHost(base.Hostname()) {
		return nil, fmt.Errorf("management bearer credentials require HTTPS except on loopback")
	}
	if cfg.Tokens == nil {
		return nil, fmt.Errorf("management client requires a service token source")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	clientCopy := *cfg.HTTPClient
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	base.Path = strings.TrimRight(base.Path, "/")
	return &httpClient{base: base, http: &clientCopy, tokens: cfg.Tokens}, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && address.IsLoopback()
}

func (c *httpClient) Provision(ctx context.Context, cmd contract.ProvisionTenant) (contract.ManagedOperation, error) {
	if err := contract.ValidateProvisionTenant(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate provision command: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/tenants", http.StatusAccepted, cmd, &result)
	if err == nil && result.Tenant != cmd.Tenant {
		err = fmt.Errorf("management operation tenant does not match provision request")
	}
	return result, err
}

func (c *httpClient) ValidatePack(ctx context.Context, manifest contract.PackManifest, files []contract.PackFile) (contract.PackValidation, error) {
	request := struct {
		Manifest contract.PackManifest `json:"manifest"`
		Files    []contract.PackFile   `json:"files"`
	}{Manifest: manifest, Files: files}
	var result contract.PackValidation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/packages/validate", http.StatusOK, request, &result)
	if err == nil && len(result.Findings) > 100 {
		err = fmt.Errorf("management package validation response exceeds 100 findings")
	}
	return result, err
}

func (c *httpClient) Inspect(ctx context.Context, tenant contract.TenantID) (contract.TenantDescriptorV2, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.TenantDescriptorV2{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	var result contract.TenantDescriptorV2
	err := c.do(ctx, http.MethodGet, "/api/management/v2/tenants/"+url.PathEscape(string(tenant)), http.StatusOK, nil, &result)
	if err == nil {
		err = contract.ValidateTenantDescriptorV2(result)
		if err == nil && result.SiteID != tenant {
			err = fmt.Errorf("management tenant response identity does not match requested tenant")
		}
	}
	return result, err
}

func (c *httpClient) InspectAlias(ctx context.Context, alias string) (contract.TenantDescriptorV2, error) {
	canonical := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(alias), "."))
	if err := contract.ValidateTenantAlias(canonical); err != nil {
		return contract.TenantDescriptorV2{}, fmt.Errorf("validate tenant alias: %w", err)
	}
	var result contract.TenantDescriptorV2
	err := c.do(ctx, http.MethodGet, "/api/management/v2/tenants/resolve?alias="+url.QueryEscape(canonical), http.StatusOK, nil, &result)
	if err == nil {
		err = contract.ValidateTenantDescriptorV2(result)
		if err == nil {
			matched := false
			for _, candidate := range result.Aliases {
				if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(candidate), "."), canonical) {
					matched = true
					break
				}
			}
			if !matched {
				err = fmt.Errorf("management tenant response does not own requested alias")
			}
		}
	}
	return result, err
}

func (c *httpClient) InspectModules(ctx context.Context, tenant contract.TenantID) (contract.ModuleInspection, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.ModuleInspection{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	var result contract.ModuleInspection
	endpoint := "/api/management/v2/tenants/" + url.PathEscape(string(tenant)) + "/modules/inspection"
	err := c.do(ctx, http.MethodGet, endpoint, http.StatusOK, nil, &result)
	return result, err
}

func (c *httpClient) InspectExecution(ctx context.Context, tenant contract.TenantID, executionID string) (contract.ExecutionInspection, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.ExecutionInspection{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	id, err := contract.ParseOperationID(executionID)
	if err != nil {
		return contract.ExecutionInspection{}, fmt.Errorf("validate execution ID: %w", err)
	}
	var result contract.ExecutionInspection
	endpoint := "/api/management/v2/tenants/" + url.PathEscape(string(tenant)) + "/executions/" + url.PathEscape(string(id))
	err = c.do(ctx, http.MethodGet, endpoint, http.StatusOK, nil, &result)
	if err == nil && (result.ExecutionID != string(id) || result.CallerModuleID == "" || result.ProviderID == "") {
		err = fmt.Errorf("management execution response identity is invalid")
	}
	return result, err
}

// PlanModuleChange returns the Engine's current lifecycle plan, including any
// dependency or open-work blockers. The caller must present those blockers
// and apply only the exact returned plan hash and base revision.
func (c *httpClient) PlanModuleChange(ctx context.Context, tenant contract.TenantID, request contract.PlanModuleChange) (contract.ModuleChangePlan, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.ModuleChangePlan{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	if err := contract.ValidatePlanModuleChange(request); err != nil {
		return contract.ModuleChangePlan{}, fmt.Errorf("validate module plan request: %w", err)
	}
	var result contract.ModuleChangePlan
	endpoint := "/api/management/v2/tenants/" + url.PathEscape(string(tenant)) + "/modules/plan"
	err := c.do(ctx, http.MethodPost, endpoint, http.StatusOK, request, &result)
	if err == nil && (result.ModuleID != request.ModuleID || result.TargetState != request.TargetState || result.TargetVersion != request.TargetVersion || !reflect.DeepEqual(result.ProviderSelections, request.ProviderSelections) || result.BaseRevision != request.ExpectedRevision || len(result.PlanHash) != 64) {
		err = fmt.Errorf("management module plan response identity is invalid")
	}
	if err == nil {
		if digest, decodeErr := hex.DecodeString(result.PlanHash); decodeErr != nil || len(digest) != 32 || strings.ToLower(result.PlanHash) != result.PlanHash {
			err = fmt.Errorf("management module plan hash is invalid")
		}
	}
	return result, err
}

// ApplyModuleChange submits the reviewed Engine plan. It never replans or
// silently substitutes a newer revision; callers poll the returned operation
// through Operation after acceptance.
func (c *httpClient) ApplyModuleChange(ctx context.Context, tenant contract.TenantID, command contract.ApplyModuleChange) (contract.ManagedOperation, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	if err := contract.ValidateApplyModuleChange(command); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate module apply command: %w", err)
	}
	var result contract.ManagedOperation
	endpoint := "/api/management/v2/tenants/" + url.PathEscape(string(tenant)) + "/modules/apply"
	err := c.do(ctx, http.MethodPost, endpoint, http.StatusAccepted, command, &result)
	if err == nil && result.Tenant != tenant {
		err = fmt.Errorf("management operation tenant does not match module apply request")
	}
	return result, err
}

func (c *httpClient) ResolveDirectoryEntry(ctx context.Context, tenant contract.TenantID) (contract.SiteDirectoryEntryV2, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.SiteDirectoryEntryV2{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	var result contract.SiteDirectoryEntryV2
	err := c.do(ctx, http.MethodGet, "/api/management/v2/directory/resolve?site_id="+url.QueryEscape(string(tenant)), http.StatusOK, nil, &result)
	if err == nil {
		err = contract.ValidateSiteDirectoryEntryV2(result)
		if err == nil && result.SiteID != tenant {
			err = fmt.Errorf("management directory response identity does not match requested tenant")
		}
	}
	return result, err
}

func (c *httpClient) ResolveDirectoryAlias(ctx context.Context, alias string) (contract.SiteDirectoryEntryV2, error) {
	canonical := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(alias), "."))
	if err := contract.ValidateTenantAlias(canonical); err != nil {
		return contract.SiteDirectoryEntryV2{}, fmt.Errorf("validate tenant alias: %w", err)
	}
	var result contract.SiteDirectoryEntryV2
	err := c.do(ctx, http.MethodGet, "/api/management/v2/directory/resolve?alias="+url.QueryEscape(canonical), http.StatusOK, nil, &result)
	if err == nil {
		err = contract.ValidateSiteDirectoryEntryV2(result)
		if err == nil {
			matched := false
			for _, candidate := range result.Aliases {
				if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(candidate), "."), canonical) {
					matched = true
					break
				}
			}
			if !matched {
				err = fmt.Errorf("management directory response does not own requested alias")
			}
		}
	}
	return result, err
}

func (c *httpClient) ApplyPackage(ctx context.Context, cmd contract.ApplyPackage) (contract.ManagedOperation, error) {
	if err := contract.ValidateApplyPackage(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate package command: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/tenants/"+url.PathEscape(string(cmd.Tenant))+"/packages", http.StatusAccepted, cmd, &result)
	if err == nil && result.Tenant != cmd.Tenant {
		err = fmt.Errorf("management operation tenant does not match package request")
	}
	return result, err
}

func (c *httpClient) InviteTenantAdministrator(ctx context.Context, tenant contract.TenantID, cmd contract.RequestTenantInvitation) (contract.ManagedOperation, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	if err := contract.ValidateRequestTenantInvitation(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant invitation: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/tenants/"+url.PathEscape(string(tenant))+"/invitation", http.StatusAccepted, cmd, &result)
	if err == nil && result.Tenant != tenant {
		err = fmt.Errorf("management operation tenant does not match invitation request")
	}
	return result, err
}

func (c *httpClient) ReconcileTenantInvitationOperation(ctx context.Context, id contract.OperationID, cmd contract.ReconcileTenantInvitationOperation) (contract.ManagedOperation, error) {
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate management operation ID: %w", err)
	}
	if err := contract.ValidateReconcileTenantInvitationOperation(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant invitation reconciliation: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/operations/"+url.PathEscape(string(id))+"/reconcile-tenant-invitation", http.StatusAccepted, cmd, &result)
	if err == nil && result.ID != id {
		err = fmt.Errorf("management reconciliation response operation ID does not match request")
	}
	return result, err
}

func (c *httpClient) SetState(ctx context.Context, tenant contract.TenantID, cmd contract.SetTenantState) (contract.ManagedOperation, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	if err := contract.ValidateSetTenantState(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant-state command: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/tenants/"+url.PathEscape(string(tenant))+"/state", http.StatusAccepted, cmd, &result)
	if err == nil && result.Tenant != tenant {
		err = fmt.Errorf("management operation tenant does not match state request")
	}
	return result, err
}

func (c *httpClient) ReconcileTenantStateOperation(ctx context.Context, id contract.OperationID, cmd contract.ReconcileTenantStateOperation) (contract.ManagedOperation, error) {
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate management operation ID: %w", err)
	}
	if err := contract.ValidateReconcileTenantStateOperation(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant-state reconciliation command: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/operations/"+url.PathEscape(string(id))+"/reconcile-tenant-state", http.StatusAccepted, cmd, &result)
	if err == nil && result.ID != id {
		err = fmt.Errorf("management reconciliation response operation ID does not match request")
	}
	return result, err
}

func (c *httpClient) ReconcileTenantProvisionOperation(ctx context.Context, id contract.OperationID, cmd contract.ReconcileTenantProvisionOperation) (contract.ManagedOperation, error) {
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate operation ID: %w", err)
	}
	if err := contract.ValidateReconcileTenantProvisionOperation(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant-provision reconciliation command: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/operations/"+url.PathEscape(string(id))+"/reconcile-tenant-provision", http.StatusAccepted, cmd, &result)
	return result, err
}

func (c *httpClient) ReconcileTenantPackageOperation(ctx context.Context, id contract.OperationID, cmd contract.ReconcileTenantPackageOperation) (contract.ManagedOperation, error) {
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate operation ID: %w", err)
	}
	if err := contract.ValidateReconcileTenantPackageOperation(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate package-application reconciliation command: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/operations/"+url.PathEscape(string(id))+"/reconcile-tenant-package", http.StatusAccepted, cmd, &result)
	if err == nil && result.ID != id {
		err = fmt.Errorf("management reconciliation response operation ID does not match request")
	}
	return result, err
}

func (c *httpClient) ReconcileTenantDeleteOperation(ctx context.Context, id contract.OperationID, cmd contract.ReconcileTenantDeleteOperation) (contract.ManagedOperation, error) {
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate operation ID: %w", err)
	}
	if err := contract.ValidateReconcileTenantDeleteOperation(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant-delete reconciliation command: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/operations/"+url.PathEscape(string(id))+"/reconcile-tenant-delete", http.StatusAccepted, cmd, &result)
	if err == nil && result.ID != id {
		err = fmt.Errorf("management reconciliation response operation ID does not match request")
	}
	return result, err
}

func (c *httpClient) Delete(ctx context.Context, tenant contract.TenantID, cmd contract.DeleteTenant) (contract.ManagedOperation, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate tenant ID: %w", err)
	}
	if err := contract.ValidateDeleteTenant(cmd); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate delete command: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodPost, "/api/management/v2/tenants/"+url.PathEscape(string(tenant))+"/delete", http.StatusAccepted, cmd, &result)
	if err == nil && result.Tenant != tenant {
		err = fmt.Errorf("management operation tenant does not match delete request")
	}
	return result, err
}

func (c *httpClient) Operation(ctx context.Context, id contract.OperationID) (contract.ManagedOperation, error) {
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return contract.ManagedOperation{}, fmt.Errorf("validate operation ID: %w", err)
	}
	var result contract.ManagedOperation
	err := c.do(ctx, http.MethodGet, "/api/management/v2/operations/"+url.PathEscape(string(id)), http.StatusOK, nil, &result)
	if err == nil && result.ID != id {
		err = fmt.Errorf("management operation response ID does not match requested operation")
	}
	return result, err
}

func (c *httpClient) Capabilities(ctx context.Context) (contract.ManagementCapabilitiesV2, error) {
	var result contract.ManagementCapabilitiesV2
	if err := c.do(ctx, http.MethodGet, "/api/management/v2/capabilities", http.StatusOK, nil, &result); err != nil {
		return contract.ManagementCapabilitiesV2{}, err
	}
	if err := contract.ValidateManagementCapabilitiesV2(result); err != nil {
		return contract.ManagementCapabilitiesV2{}, fmt.Errorf("validate Engine management capabilities: %w", err)
	}
	return result, nil
}

func (c *httpClient) do(ctx context.Context, method, endpoint string, expected int, input, output any) error {
	if ctx == nil {
		return fmt.Errorf("management request requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("get management service token: %w", err)
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("management service token is empty or invalid")
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode management request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	relative, err := url.Parse(endpoint)
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.Fragment != "" {
		return fmt.Errorf("invalid management endpoint")
	}
	endpointURL := *c.base
	endpointURL.Path = path.Join(c.base.Path, relative.Path)
	endpointURL.RawQuery = relative.RawQuery
	request, err := http.NewRequestWithContext(ctx, method, endpointURL.String(), body)
	if err != nil {
		return fmt.Errorf("create management request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("send management request: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read management response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("management response exceeds %d bytes", maxResponseBytes)
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
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("decode management response envelope: %w", err)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("management response omitted data")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("decode management response envelope: trailing data")
	}
	dataDecoder := json.NewDecoder(bytes.NewReader(envelope.Data))
	dataDecoder.DisallowUnknownFields()
	if err := dataDecoder.Decode(output); err != nil {
		return fmt.Errorf("decode management response data: %w", err)
	}
	if err := dataDecoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("decode management response data: trailing data")
	}
	if operation, ok := output.(*contract.ManagedOperation); ok {
		if err := contract.ValidateManagedOperation(*operation); err != nil {
			return fmt.Errorf("validate management operation response: %w", err)
		}
	}
	return nil
}

type RemoteError struct {
	Status int
	Body   *contract.Error
}

func (e *RemoteError) Error() string {
	if e == nil || e.Body == nil {
		return "Engine management request failed"
	}
	return fmt.Sprintf("Engine management request failed (%d, %s): %s", e.Status, e.Body.Type, e.Body.Message)
}

func decodeRemoteError(status int, data []byte) error {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Error) == 0 {
		return &RemoteError{Status: status, Body: &contract.Error{Type: contract.CodeInternal, Message: http.StatusText(status)}}
	}
	var body contract.Error
	if err := json.Unmarshal(envelope.Error, &body); err == nil && body.Type != "" {
		return &RemoteError{Status: status, Body: &body}
	}
	var legacy struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(envelope.Error, &legacy); err != nil || legacy.Code == "" {
		return &RemoteError{Status: status, Body: &contract.Error{Type: contract.CodeInternal, Message: http.StatusText(status)}}
	}
	return &RemoteError{Status: status, Body: &contract.Error{Type: contract.NormalizeCode(legacy.Code), Message: legacy.Message}}
}

var _ Client = (*httpClient)(nil)
