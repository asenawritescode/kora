package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestClientUsesTypedManagementRoutesAndServiceBearer(t *testing.T) {
	tenant := contract.TenantID("tenant-a")
	operationID := contract.OperationID(contract.NewID())
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer scoped-service-token" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept header = %q", r.Header.Get("Accept"))
		}
		var data any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/api/management/v2/capabilities":
			data = contract.ManagementCapabilitiesV2{Version: 2, Capabilities: []contract.ManagementCapability{
				contract.ManagementCapabilityOperationRead,
				contract.ManagementCapabilityTenantProvision,
				contract.ManagementCapabilityTenantProvisionRetry,
			}}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/packages/validate":
			data = contract.PackValidation{Valid: true, Findings: []contract.PackFinding{}}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/tenants":
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/api/management/v2/tenants/tenant-a":
			data = contract.TenantDescriptorV2{Version: 2, SiteID: tenant, Status: contract.TenantActive, ConfigRevision: "1", DirectoryRevision: "2", Deployment: "deployment-a"}
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/api/management/v2/tenants/resolve":
			if r.URL.Query().Get("alias") != "tenant.example.test" {
				t.Errorf("resolve alias query = %q", r.URL.Query().Get("alias"))
			}
			data = contract.TenantDescriptorV2{Version: 2, SiteID: tenant, Aliases: []string{"tenant.example.test"}, Status: contract.TenantActive, ConfigRevision: "1", DirectoryRevision: "2", Deployment: "deployment-a"}
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/api/management/v2/directory/resolve":
			if r.URL.Query().Get("alias") != "tenant.example.test" && r.URL.Query().Get("site_id") != string(tenant) {
				t.Errorf("directory selector query = %q", r.URL.RawQuery)
			}
			data = contract.SiteDirectoryEntryV2{Version: 2, SiteID: tenant, Aliases: []string{"tenant.example.test"}, Status: "active", ConfigRevision: "1", DirectoryRevision: "19", DeploymentID: "deployment-b"}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/tenants/tenant-a/packages":
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/tenants/tenant-a/state":
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/tenants/tenant-a/invitation":
			var command contract.RequestTenantInvitation
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil || command.Email != "admin@tenant.example.test" {
				t.Errorf("invitation command=%+v error=%v", command, err)
			}
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/operations/"+string(operationID)+"/reconcile-tenant-invitation":
			var command contract.ReconcileTenantInvitationOperation
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil || command.Outcome != contract.InvitationNotDelivered {
				t.Errorf("invitation reconciliation command=%+v error=%v", command, err)
			}
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/operations/"+string(operationID)+"/reconcile-tenant-state":
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/operations/"+string(operationID)+"/reconcile-tenant-provision":
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/operations/"+string(operationID)+"/reconcile-tenant-package":
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/operations/"+string(operationID)+"/reconcile-tenant-delete":
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/api/management/v2/tenants/tenant-a/delete":
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRequested}
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/api/management/v2/operations/"+string(operationID):
			data = contract.ManagedOperation{ID: operationID, Tenant: tenant, State: contract.OperationRunning}
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path != "/proxy/api/management/v2/packages/validate" {
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL + "/proxy", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "scoped-service-token", nil })})
	if err != nil {
		t.Fatal("NewClient:", err)
	}
	ctx := context.Background()
	discovery, ok := client.(CapabilitiesClient)
	if !ok {
		t.Fatal("supported HTTP client does not implement CapabilitiesClient")
	}
	capabilities, err := discovery.Capabilities(ctx)
	if err != nil || capabilities.Version != 2 || len(capabilities.Capabilities) != 3 {
		t.Fatalf("Capabilities() = %+v, %v", capabilities, err)
	}
	validation, err := client.ValidatePack(ctx, contract.PackManifest{SchemaVersion: 1, Name: "inventory", Version: "1.0.0"}, []contract.PackFile{{Path: "doctypes/stock.yaml", Content: "name: Stock", ContentType: "doctype"}})
	if err != nil || !validation.Valid {
		t.Fatalf("ValidatePack = %+v, %v", validation, err)
	}
	meta := contract.CommandMeta{IdempotencyKey: "cloud-operation-01"}
	provision := contract.ProvisionTenant{Meta: meta, Tenant: tenant, BootstrapAdminEmail: "admin@tenant.example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "tenant.example.test", Deployment: "deployment-a", DatabaseAllocationRef: "mysql:allocation-a"}
	if _, err := client.Provision(ctx, provision); err != nil {
		t.Fatal("Provision:", err)
	}
	if descriptor, err := client.Inspect(ctx, tenant); err != nil || descriptor.SiteID != tenant {
		t.Fatalf("Inspect = %+v, %v", descriptor, err)
	}
	if descriptor, err := client.InspectAlias(ctx, " TENANT.EXAMPLE.TEST. "); err != nil || descriptor.SiteID != tenant {
		t.Fatalf("InspectAlias = %+v, %v", descriptor, err)
	}
	directoryClient, ok := client.(DeploymentDirectoryClient)
	if !ok {
		t.Fatal("supported HTTP client does not implement DeploymentDirectoryClient")
	}
	if entry, err := directoryClient.ResolveDirectoryEntry(ctx, tenant); err != nil || entry.DeploymentID != "deployment-b" || entry.DirectoryRevision != "19" {
		t.Fatalf("ResolveDirectoryEntry = %+v, %v", entry, err)
	}
	if entry, err := directoryClient.ResolveDirectoryAlias(ctx, " TENANT.EXAMPLE.TEST. "); err != nil || entry.SiteID != tenant {
		t.Fatalf("ResolveDirectoryAlias = %+v, %v", entry, err)
	}
	apply := contract.ApplyPackage{Meta: meta, Tenant: tenant, ArtifactRef: "artifact:pack-a", Digest: strings.Repeat("a", 64), ExpectedConfigRevision: "1"}
	if _, err := client.ApplyPackage(ctx, apply); err != nil {
		t.Fatal("ApplyPackage:", err)
	}
	invitations, ok := client.(TenantInvitationClient)
	if !ok {
		t.Fatal("supported HTTP client does not implement TenantInvitationClient")
	}
	if _, err := invitations.InviteTenantAdministrator(ctx, tenant, contract.RequestTenantInvitation{Meta: meta, Email: "admin@tenant.example.test"}); err != nil {
		t.Fatal("InviteTenantAdministrator:", err)
	}
	recovery, ok := client.(TenantInvitationRecoveryClient)
	if !ok {
		t.Fatal("supported HTTP client does not implement TenantInvitationRecoveryClient")
	}
	if _, err := recovery.ReconcileTenantInvitationOperation(ctx, operationID, contract.ReconcileTenantInvitationOperation{Meta: meta, Outcome: contract.InvitationNotDelivered, Reason: "provider logs confirm the message was not accepted"}); err != nil {
		t.Fatal("ReconcileTenantInvitationOperation:", err)
	}
	if _, err := client.SetState(ctx, tenant, contract.SetTenantState{Meta: meta, State: contract.TenantSuspended, ExpectedConfigRevision: "1", Reason: "operator request"}); err != nil {
		t.Fatal("SetState:", err)
	}
	if _, err := client.ReconcileTenantStateOperation(ctx, operationID, contract.ReconcileTenantStateOperation{Meta: meta, Reason: "operator verified current status"}); err != nil {
		t.Fatal("ReconcileTenantStateOperation:", err)
	}
	if _, err := client.ReconcileTenantProvisionOperation(ctx, operationID, contract.ReconcileTenantProvisionOperation{Meta: meta, Reason: "operator verified tenant identity and allocation"}); err != nil {
		t.Fatal("ReconcileTenantProvisionOperation:", err)
	}
	if _, err := client.ReconcileTenantPackageOperation(ctx, operationID, contract.ReconcileTenantPackageOperation{Meta: meta, Reason: "operator inspected package activation journal"}); err != nil {
		t.Fatal("ReconcileTenantPackageOperation:", err)
	}
	if _, err := client.ReconcileTenantDeleteOperation(ctx, operationID, contract.ReconcileTenantDeleteOperation{Meta: meta, Reason: "operator verified immutable checkpoint and allocation"}); err != nil {
		t.Fatal("ReconcileTenantDeleteOperation:", err)
	}
	if _, err := client.Delete(ctx, tenant, contract.DeleteTenant{Meta: meta, RecoveryCheckpointRef: "checkpoint:backup-a"}); err != nil {
		t.Fatal("Delete:", err)
	}
	if _, err := client.Operation(ctx, operationID); err != nil {
		t.Fatal("Operation:", err)
	}
	if calls != 17 {
		t.Fatalf("HTTP calls = %d, want all management methods and capability discovery", calls)
	}
}

func TestClientValidatesBeforeSendingAndChecksResponseIdentity(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"version":2,"site_id":"other-tenant","status":"active","config_revision":"1","directory_revision":"1","deployment_id":"deployment-a"}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Provision(context.Background(), contract.ProvisionTenant{}); err == nil {
		t.Fatal("invalid provision command was sent")
	}
	if calls != 0 {
		t.Fatalf("invalid command made %d HTTP calls", calls)
	}
	if _, err := client.Inspect(context.Background(), "tenant-a"); err == nil {
		t.Fatal("mismatched tenant identity was accepted")
	}
	if calls != 1 {
		t.Fatalf("Inspect calls = %d, want 1", calls)
	}
}

func TestClientMapsStructuredAndLegacyErrorsAndBoundsBody(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		code contract.Code
	}{
		{name: "v2", body: `{"error":{"type":"PERMISSION_DENIED","message":"denied"}}`, code: contract.CodePermissionDenied},
		{name: "current api", body: `{"error":{"code":"NOT_FOUND","message":"missing"}}`, code: contract.CodeNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewClient(Config{BaseURL: server.URL, Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Inspect(context.Background(), "tenant-a")
			var remote *RemoteError
			if !errors.As(err, &remote) || remote.Status != http.StatusForbidden || remote.Body.Type != test.code {
				t.Fatalf("remote error = %#v, want code %s", err, test.code)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":"`+strings.Repeat("x", maxResponseBytes)+`"}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(context.Background(), "tenant-a"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestNewClientRejectsUnsafeBaseURLAndMissingTokenSource(t *testing.T) {
	for _, base := range []string{"", "relative/path", "ftp://engine.test", "https://user:pass@engine.test", "https://engine.test/path?token=x", "https://engine.test/#fragment", "http://engine.test"} {
		if _, err := NewClient(Config{BaseURL: base, Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })}); err == nil {
			t.Errorf("unsafe base URL %q was accepted", base)
		}
	}
	if _, err := NewClient(Config{BaseURL: "https://engine.test"}); err == nil {
		t.Fatal("missing service token source was accepted")
	}
}

func TestClientDoesNotFollowManagementRedirects(t *testing.T) {
	var destinationCalls int
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls++
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(context.Background(), "tenant-a"); err == nil {
		t.Fatal("management redirect was followed or accepted")
	}
	if destinationCalls != 0 {
		t.Fatalf("management client followed redirect %d times", destinationCalls)
	}
}
