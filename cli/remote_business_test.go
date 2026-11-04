package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/spf13/cobra"
)

func cliTestCatalog(scope contract.BusinessScope) contract.ApplicationCatalog {
	in := contract.SchemaRef{ID: "sales.input", Version: 1}
	out := contract.SchemaRef{ID: "sales.output", Version: 1}
	return contract.ApplicationCatalog{
		Revision: 7, BusinessScope: scope,
		Modules:              []contract.ApplicationCatalogModule{{ID: "sales.orders", Version: "1.0.0", State: string(contract.ModuleActive)}},
		PublishedQueries:     []contract.ApplicationCatalogQuery{{OwnerModuleID: "sales.orders", Name: "list", Route: "/api/v1/modules/sales.orders/queries/list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"id"}, PageLimit: 50}},
		PublishedProjections: []contract.ApplicationCatalogProjection{},
		PublishedOperations:  []contract.ApplicationCatalogOperation{{OperationRef: contract.PublishedOperationRef("sales.orders", "sales.invoice", "issue"), OwnerModuleID: "sales.orders", Capability: "sales.invoice", Operation: "issue", InputSchema: in, OutputSchema: out, Availability: contract.OperationAvailable, Permissions: []string{"invoice.issue"}}},
		Schemas:              []contract.ModuleSchemaDocument{{Ref: in, Schema: json.RawMessage(`{"type":"object"}`)}, {Ref: out, Schema: json.RawMessage(`{"type":"object"}`)}},
		AllowedEntityChoices: []contract.BusinessScope{}, Blockers: []contract.ModuleBlocker{},
	}
}

func runCLICommand(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err != nil && strings.TrimSpace(stderr.String()) != "" {
		t.Logf("command stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

func TestRemoteBusinessCommandsUseScopedSDKAndWaitDoesNotResubmit(t *testing.T) {
	const token = "actor-token-test-value"
	t.Setenv("KORA_REMOTE_TEST_TOKEN", token)
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	catalog := cliTestCatalog(scope)
	operationRef := contract.PublishedOperationRef("sales.orders", "sales.invoice", "issue")
	executionCalls := 0
	var queryBody contract.PublishedQueryRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Kora-Legal-Entity-ID") != "entity-a" {
			t.Errorf("authorization/scope headers = %q / %q", r.Header.Get("Authorization"), r.Header.Get("X-Kora-Legal-Entity-ID"))
		}
		w.Header().Set("Content-Type", "application/json")
		var data any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/s/demo/api/v2/modules/catalog":
			data = catalog
		case r.Method == http.MethodPost && r.URL.Path == "/s/demo/api/v1/modules/sales.orders/queries/list":
			if err := json.NewDecoder(r.Body).Decode(&queryBody); err != nil {
				t.Error(err)
			}
			data = contract.QueryResult{Data: json.RawMessage(`{"items":[]}`), AsOf: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), Freshness: contract.QueryFresh}
		case r.Method == http.MethodPost && r.URL.Path == "/s/demo/api/v2/modules/operations/"+operationRef+"/execute":
			executionCalls++
			var request contract.ExecutePublishedOperation
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Meta.IdempotencyKey != "intent-1" || request.InputSchema.ID != "sales.input" {
				t.Errorf("execute request=%+v err=%v", request, err)
			}
			data = contract.OperationReceipt{ExecutionID: "exec-1", OperationRef: operationRef, BusinessScope: scope, Status: contract.ReceiptAccepted, Revision: "7"}
		case r.Method == http.MethodGet && r.URL.Path == "/s/demo/api/v2/module-executions/exec-1":
			data = contract.OperationReceipt{ExecutionID: "exec-1", OperationRef: operationRef, BusinessScope: scope, Status: contract.ReceiptCompleted, OutputSchema: &outSchema, Output: json.RawMessage(`{"invoice_id":"INV-1"}`), Revision: "8"}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "remote.json")
	if err := saveRemoteProfiles(configPath, remoteProfileFile{Current: "work", Profiles: map[string]remoteProfile{"work": {URL: server.URL, Site: "demo", TokenEnv: "KORA_REMOTE_TEST_TOKEN", TenantID: "tenant-a", LegalEntityID: "entity-a"}}}); err != nil {
		t.Fatal(err)
	}
	opts := &remoteOptions{Config: configPath, Timeout: time.Second}
	queryOutput, err := runCLICommand(t, newRemoteQueryCommand(opts), "sales.orders", "list", "--input", `{"status":"open"}`, "--limit", "10")
	if err != nil {
		t.Fatal(err)
	}
	if queryBody.Limit != 10 || string(queryBody.Input) != `{"status":"open"}` {
		t.Fatalf("query payload=%+v", queryBody)
	}
	if !strings.Contains(queryOutput, `"freshness": "fresh"`) {
		t.Fatalf("query output lost metadata: %s", queryOutput)
	}

	operation := newRemoteOperationCommand(opts)
	output, err := runCLICommand(t, operation, "execute", operationRef, "--input", `{"amount":1}`, "--idempotency-key", "intent-1", "--wait", "--poll-interval", "1ms")
	if err != nil {
		t.Fatal(err)
	}
	if executionCalls != 1 {
		t.Fatalf("operation execution calls = %d, want one despite receipt polling", executionCalls)
	}
	if !strings.Contains(output, `"status": "completed"`) || !strings.Contains(output, `"invoice_id": "INV-1"`) {
		t.Fatalf("canonical receipt not preserved: %s", output)
	}
}

var outSchema = contract.SchemaRef{ID: "sales.output", Version: 1}

func TestAdminManagementProfileIsSeparateAndAppliesExactPlan(t *testing.T) {
	const managementToken = "management-token-test-value"
	t.Setenv("KORA_MANAGEMENT_TEST_TOKEN", managementToken)
	var sawApply bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+managementToken {
			t.Errorf("management authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		var data any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/management/v2/tenants/tenant-a/modules/plan":
			var request contract.PlanModuleChange
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			data = contract.ModuleChangePlan{ModuleID: request.ModuleID, TargetState: request.TargetState, BaseRevision: request.ExpectedRevision, PlanHash: strings.Repeat("a", 64), DependencyBlockers: []contract.ModuleBlocker{}, OpenWorkBlockers: []contract.ModuleBlocker{}}
		case r.Method == http.MethodPost && r.URL.Path == "/api/management/v2/tenants/tenant-a/modules/apply":
			var request contract.ApplyModuleChange
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.PlanHash != strings.Repeat("a", 64) || request.ExpectedRevision != 7 || request.Meta.IdempotencyKey != "apply-1" {
				t.Errorf("apply changed approved plan: %+v", request)
			}
			sawApply = true
			data = contract.ManagedOperation{ID: "managed-op-1", Tenant: "tenant-a", State: contract.OperationRequested}
		case r.Method == http.MethodGet && r.URL.Path == "/api/management/v2/operations/managed-op-1":
			data = contract.ManagedOperation{ID: "managed-op-1", Tenant: "tenant-a", State: contract.OperationSucceeded}
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost && !strings.HasSuffix(r.URL.Path, "/modules/plan") {
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "admin.json")
	if err := saveRemoteProfiles(configPath, remoteProfileFile{Current: "prod-admin", Profiles: map[string]remoteProfile{"prod-admin": {URL: server.URL, TokenEnv: "KORA_MANAGEMENT_TEST_TOKEN"}}}); err != nil {
		t.Fatal(err)
	}
	admin := newAdminCommand()
	// Use a separate options instance with an explicit management profile file.
	adminOpts := &adminOptions{Config: configPath, Timeout: time.Second}
	planCommand := newAdminModulesCommand(adminOpts)
	planJSON, err := runCLICommand(t, planCommand, "plan", "tenant-a", "sales.orders", "--target-state", "draining", "--expected-revision", "7")
	if err != nil {
		t.Fatal(err)
	}
	planFile := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planFile, []byte(planJSON), 0600); err != nil {
		t.Fatal(err)
	}
	applyCommand := newAdminModulesCommand(adminOpts)
	output, err := runCLICommand(t, applyCommand, "apply", "tenant-a", "--plan-file", planFile, "--idempotency-key", "apply-1", "--wait", "--poll-interval", "1ms")
	if err != nil {
		t.Fatal(err)
	}
	if !sawApply || !strings.Contains(output, `"state": "succeeded"`) {
		t.Fatalf("apply/poll output=%s sawApply=%v", output, sawApply)
	}
	if admin == nil {
		t.Fatal("admin root command was not constructed")
	}
}

func TestCLIMachineExitMappingAndCredentialRedaction(t *testing.T) {
	unknown := contract.OperationReceipt{Status: contract.ReceiptOutcomeUnknown}
	var exit *CLIExitError
	if err := receiptExit(unknown); !errors.As(err, &exit) || exit.Code != ExitUnknown {
		t.Fatalf("unknown receipt exit = %#v", err)
	}
	if err := classifyTenantError(errors.New("connection reset"), ""); !errors.As(err, &exit) || exit.Code != ExitUnavailable {
		t.Fatalf("tenant transport exit = %#v", err)
	}
	if err := classifyManagementError(errors.New("connection reset"), ""); !errors.As(err, &exit) || exit.Code != ExitUnavailable {
		t.Fatalf("management transport exit = %#v", err)
	}
	if got := safeRemoteMessage(errors.New("token secret-token rejected"), "secret-token"); strings.Contains(got, "secret-token") {
		t.Fatalf("credential was not redacted: %s", got)
	}
}

func TestStrictCLIJSONRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	var subject contract.SubjectRef
	for _, input := range []string{`{"kind":"sales.order","id":"1","revision":"1","owner":"forged"}`, `{"kind":"sales.order","id":"1","revision":"1"}{}`} {
		if err := decodeStrictJSON([]byte(input), &subject); err == nil {
			t.Fatalf("accepted non-canonical JSON input %s", input)
		}
	}
}

func TestAdminModulePlanSurfacesLifecycleBlockersWithConflictExit(t *testing.T) {
	const token = "blocked-plan-token"
	t.Setenv("KORA_BLOCKED_PLAN_TOKEN", token)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("management authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": contract.ModuleChangePlan{
			ModuleID: "sales.orders", TargetState: contract.ModuleDraining, BaseRevision: 7,
			PlanHash:         strings.Repeat("a", 64),
			OpenWorkBlockers: []contract.ModuleBlocker{{Code: "workflow.outcome_unknown", Count: 1}},
		}})
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "admin.json")
	if err := saveRemoteProfiles(configPath, remoteProfileFile{Current: "operator", Profiles: map[string]remoteProfile{"operator": {URL: server.URL, TokenEnv: "KORA_BLOCKED_PLAN_TOKEN"}}}); err != nil {
		t.Fatal(err)
	}
	output, err := runCLICommand(t, newAdminModulesCommand(&adminOptions{Config: configPath, Timeout: time.Second}), "plan", "tenant-a", "sales.orders", "--target-state", "draining", "--expected-revision", "7")
	var exit *CLIExitError
	if !errors.As(err, &exit) || exit.Code != ExitConflict || !strings.Contains(output, "workflow.outcome_unknown") {
		t.Fatalf("blocked plan output/exit = %s / %v", output, err)
	}
}

func TestBusinessProfileRejectsManagementCredentialClass(t *testing.T) {
	t.Setenv("KORA_MANAGEMENT_TOKEN", "never-send-to-business-api")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("business request should not be sent") }))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "remote.json")
	if err := saveRemoteProfiles(configPath, remoteProfileFile{Current: "work", Profiles: map[string]remoteProfile{"work": {URL: server.URL, Site: "demo", TokenEnv: "KORA_MANAGEMENT_TOKEN", TenantID: "tenant-a"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&remoteOptions{Config: configPath}).businessClient(); err == nil {
		t.Fatal("management credential class was accepted for tenant business API")
	}
}

func TestBusinessCommandsUseSavedActorSession(t *testing.T) {
	const sessionToken = "saved-actor-session"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+sessionToken {
			t.Errorf("authorization = %q, want saved actor session", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": contract.ApplicationCatalog{Revision: 1, BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}}})
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "remote.json")
	if err := saveRemoteProfiles(configPath, remoteProfileFile{Current: "work", Profiles: map[string]remoteProfile{"work": {URL: server.URL, Site: "demo", TokenEnv: "KORA_REMOTE_TEST_SESSION", TenantID: "tenant-a"}}}); err != nil {
		t.Fatal(err)
	}
	if err := saveRemoteCredential(configPath, "work", remoteCredential{URL: server.URL, Site: "demo", Session: sessionToken, Expires: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client, err := (&remoteOptions{Config: configPath, Timeout: time.Second}).businessClient()
	if err != nil {
		t.Fatal("load saved actor session:", err)
	}
	if _, err := client.Catalog(t.Context()); err != nil {
		t.Fatal("call published catalog with saved actor session:", err)
	}
}

func TestAdminDoesNotReadRemoteBusinessProfile(t *testing.T) {
	remotePath := filepath.Join(t.TempDir(), "remote.json")
	if err := saveRemoteProfiles(remotePath, remoteProfileFile{Current: "user", Profiles: map[string]remoteProfile{"user": {URL: "https://example.test", TokenEnv: "KORA_REMOTE_TOKEN"}}}); err != nil {
		t.Fatal(err)
	}
	admin := &adminOptions{Config: filepath.Join(t.TempDir(), "admin.json")}
	if _, err := admin.client(); err == nil {
		t.Fatal("admin client reused remote business profile")
	}
	if _, err := (&remoteOptions{Config: remotePath}).businessClient(); err == nil {
		t.Fatal("business client accepted profile without canonical tenant/site")
	}
}

func TestRemoteWorkflowCommandsUseTypedScopedRoutesAndStableKeys(t *testing.T) {
	const token = "workflow-token-test"
	t.Setenv("KORA_WORKFLOW_TEST_TOKEN", token)
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	instance := contract.WorkflowInstance{ID: "workflow-1", Revision: "4", DefinitionID: "sales.approval", DefinitionVersion: 2, Subject: contract.SubjectRef{Kind: "sales.order", ID: "order-1", Revision: "3"}, BusinessScope: scope, Status: contract.InstanceWaiting, CurrentStep: "approval", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	var startKey, cancelKey, reconcileKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Kora-Legal-Entity-ID") != "entity-a" {
			t.Errorf("credential/scope header=%q/%q", r.Header.Get("Authorization"), r.Header.Get("X-Kora-Legal-Entity-ID"))
		}
		w.Header().Set("Content-Type", "application/json")
		var data any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/s/demo/api/v2/workflow-instances":
			var request contract.StartWorkflow
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			startKey = string(request.Meta.IdempotencyKey)
			data = instance
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/s/demo/api/v2/workflow-instances/workflow-1":
			data = instance
		case r.Method == http.MethodGet && r.URL.Path == "/s/demo/api/v2/workflow-instances/workflow-1/history":
			data = []contract.WorkflowHistoryEntry{{Sequence: 1, StepID: "approval", Kind: "step.started", Status: "waiting", CreatedAt: time.Now().UTC()}}
		case r.Method == http.MethodPost && r.URL.Path == "/s/demo/api/v2/workflow-instances/workflow-1/cancel":
			var request contract.CancelWorkflow
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			cancelKey = string(request.Meta.IdempotencyKey)
			instance.Status = contract.InstanceCancelled
			data = instance
		case r.Method == http.MethodPost && r.URL.Path == "/s/demo/api/v2/workflow-instances/workflow-1/reconcile":
			var request contract.ReconcileWorkflow
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			reconcileKey = string(request.Meta.IdempotencyKey)
			data = instance
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "remote.json")
	if err := saveRemoteProfiles(configPath, remoteProfileFile{Current: "work", Profiles: map[string]remoteProfile{"work": {URL: server.URL, Site: "demo", TokenEnv: "KORA_WORKFLOW_TEST_TOKEN", TenantID: "tenant-a", LegalEntityID: "entity-a"}}}); err != nil {
		t.Fatal(err)
	}
	opts := &remoteOptions{Config: configPath, Timeout: time.Second}
	workflow := newRemoteWorkflowCommand(opts)
	if _, err := runCLICommand(t, workflow, "start", "--definition", "sales.approval", "--version", "2", "--subject", `{"kind":"sales.order","id":"order-1","revision":"3"}`, "--idempotency-key", "start-1"); err == nil {
		t.Fatal("waiting workflow should return accepted/pending exit code")
	} else {
		var exit *CLIExitError
		if !errors.As(err, &exit) || exit.Code != ExitAccepted {
			t.Fatalf("workflow start exit=%v", err)
		}
	}
	if startKey != "start-1" {
		t.Fatalf("workflow start key=%q", startKey)
	}
	if _, err := runCLICommand(t, newRemoteWorkflowCommand(opts), "history", "workflow-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLICommand(t, newRemoteWorkflowCommand(opts), "cancel", "workflow-1", "--idempotency-key", "cancel-1", "--reason", "operator request"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLICommand(t, newRemoteWorkflowCommand(opts), "reconcile", "workflow-1", "--idempotency-key", "reconcile-1", "--step-id", "approval", "--outcome", "abandon", "--reason", "no independent evidence", "--evidence-ref", "audit:case-7"); err != nil {
		t.Fatal(err)
	}
	if cancelKey != "cancel-1" || reconcileKey != "reconcile-1" {
		t.Fatalf("workflow command keys cancel=%q reconcile=%q", cancelKey, reconcileKey)
	}
}
