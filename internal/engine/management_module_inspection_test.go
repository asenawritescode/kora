package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	knet "github.com/asenawritescode/kora/net"
)

func TestManagementModuleInspectorReturnsEmptyTenantSnapshot(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?")).WithArgs("tenant-inspection").WillReturnError(sql.ErrNoRows)
	router := knet.NewSiteRouter([]*knet.LoadedSite{{SiteID: "tenant-inspection", DB: database, DBType: "mysql"}})
	executor, err := newManagementModuleLifecycleExecutor(router)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := executor.InspectModules(context.Background(), "tenant-inspection")
	if err != nil {
		t.Fatal("inspect empty tenant module snapshot:", err)
	}
	if inspection.ConfigRevision != 0 || len(inspection.Installations) != 0 || len(inspection.ResourceOwners) != 0 || len(inspection.Bindings) != 0 || len(inspection.CapabilityAvailability) != 0 || len(inspection.BlockingReasons) != 0 {
		t.Fatalf("empty tenant module inspection = %+v", inspection)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestManagementExecutionInspectionIsTenantScopedAndRedacted(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	executionID := contract.NewID()
	dispatch := contract.PinnedDispatch{ExecutionID: executionID, Scope: contract.BusinessScope{TenantID: "tenant-inspection", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.ar", ProviderID: "ar-provider", BindingRevision: 7, Operation: contract.CapabilityAction{CapabilityName: "ar.invoice", OperationName: "issue", InputSchema: contract.SchemaRef{ID: "ar.input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "ar.output", Version: 1}}, IdempotencyKey: "stable-key", InputHash: strings.Repeat("a", 64)}
	if err := dispatch.Validate(); err != nil {
		t.Fatal("build pinned inspection fixture:", err)
	}
	payload, err := json.Marshal(map[string]any{"dispatch": dispatch, "lease": map[string]any{"credential_ref": "credential-must-not-leak", "grant_epoch": 1}})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pinned_dispatch_json,status,instance_id,step_id FROM _kora_workflow_attempt WHERE tenant_id=? AND execution_id=? ORDER BY attempt DESC LIMIT 1")).WithArgs("tenant-inspection", executionID).WillReturnRows(sqlmock.NewRows([]string{"pinned_dispatch_json", "status", "instance_id", "step_id"}).AddRow(string(payload), "outcome_unknown", "workflow-1", "issue-invoice"))
	router := knet.NewSiteRouter([]*knet.LoadedSite{{SiteID: "tenant-inspection", DB: database, DBType: "mysql"}})
	executor, err := newManagementModuleLifecycleExecutor(router)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := executor.InspectExecution(context.Background(), "tenant-inspection", executionID)
	if err != nil {
		t.Fatal("inspect pinned workflow execution:", err)
	}
	if inspection.CallerModuleID != dispatch.CallerModuleID || inspection.OwnerModuleID != dispatch.ProviderModuleID || inspection.ProviderID != dispatch.ProviderID || inspection.BindingRevision != dispatch.BindingRevision || inspection.DeliveryState != "outcome_unknown" || inspection.ReconciliationState != "required" || inspection.CorrelationID != "workflow-1/issue-invoice" {
		t.Fatalf("pinned execution inspection = %+v", inspection)
	}
	encoded, _ := json.Marshal(inspection)
	if strings.Contains(string(encoded), "credential-must-not-leak") || strings.Contains(string(encoded), "credential_ref") || strings.Contains(string(encoded), "input_hash") || strings.Contains(string(encoded), "stable-key") {
		t.Fatalf("execution inspection leaked private dispatch data: %s", encoded)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestManagementExecutionInspectionDoesNotCrossTenant(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pinned_dispatch_json,status,instance_id,step_id FROM _kora_workflow_attempt WHERE tenant_id=? AND execution_id=? ORDER BY attempt DESC LIMIT 1")).WithArgs("tenant-inspection", "execution-private").WillReturnError(sql.ErrNoRows)
	router := knet.NewSiteRouter([]*knet.LoadedSite{{SiteID: "tenant-inspection", DB: database, DBType: "mysql"}})
	executor, err := newManagementModuleLifecycleExecutor(router)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.InspectExecution(context.Background(), "tenant-inspection", "execution-private"); err == nil {
		t.Fatal("missing execution was exposed")
	}
	if _, err := executor.InspectExecution(context.Background(), "tenant-other", "execution-private"); err == nil {
		t.Fatal("another tenant execution was exposed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleInspectionCountAdditionSaturates(t *testing.T) {
	if addBoundedCount(^uint32(0)-1, 4) != ^uint32(0) || addBoundedCount(4, 3) != 7 {
		t.Fatal("module pending-work count overflowed or failed to accumulate")
	}
}
