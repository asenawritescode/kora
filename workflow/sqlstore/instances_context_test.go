package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestWorkflowStartAdmissionUsesModuleLifecycleFence(t *testing.T) {
	for _, state := range []string{string(contract.ModuleActive), string(contract.ModuleDraining), string(contract.ModuleDisabled)} {
		t.Run(state, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			mock.ExpectBegin()
			tx, err := database.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "finance.ar", State: state}}})
			mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?")).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id=?")).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
			err = admitWorkflowModuleActive(context.Background(), tx, db.Resolve("libsql"), "tenant-a", "finance.ar")
			if state == string(contract.ModuleActive) {
				if err != nil {
					t.Fatalf("active module workflow start denied: %v", err)
				}
			} else {
				var domainErr *contract.Error
				if !errors.As(err, &domainErr) || domainErr.Type != contract.CodePermissionDenied {
					t.Fatalf("%s workflow start error=%v, want permission denied", state, err)
				}
			}
			_ = tx.Rollback()
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectOpenWorkCountsOnlyOwnedTenantWorkflows(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewInstanceStore(database, &db.LibSQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=? AND i.status NOT IN ('completed','cancelled','failed') AND json_extract(d.definition_json,'$.owner_module_id')=?`)).WithArgs("tenant-a", "finance.ar").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT i.id FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=? AND i.status NOT IN ('completed','cancelled','failed') AND json_extract(d.definition_json,'$.owner_module_id')=? ORDER BY i.updated_at,i.id LIMIT ?`)).WithArgs("tenant-a", "finance.ar", 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("opaque-instance"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_workflow_attempt WHERE tenant_id=? AND status IN ('running','outcome_unknown') AND pinned_dispatch_json IS NOT NULL AND (json_extract(pinned_dispatch_json,'$.dispatch.caller_module_id')=? OR json_extract(pinned_dispatch_json,'$.dispatch.provider_module_id')=?)`)).WithArgs("tenant-a", "finance.ar", "finance.ar").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_workflow_attempt a JOIN _kora_workflow_instance i ON i.tenant_id=a.tenant_id AND i.id=a.instance_id JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE a.tenant_id=? AND a.status='outcome_unknown' AND json_extract(d.definition_json,'$.owner_module_id')=?`)).WithArgs("tenant-a", "finance.ar").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	blockers, err := store.InspectOpenWork(context.Background(), "tenant-a", "finance.ar", 1)
	if err != nil || len(blockers) != 2 || blockers[0].Code != "workflow_instance_open" || blockers[0].Count != 2 || len(blockers[0].Examples) != 1 || blockers[0].Examples[0] != "opaque-instance" || blockers[1].Code != "external_outcome_unknown" || blockers[1].Count != 1 {
		t.Fatalf("workflow blockers = %+v, %v", blockers, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectOpenWorkUsesPostgresJSONOwnerSelector(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewInstanceStore(database, &db.PostgresDialect{})
	if err != nil {
		t.Fatal(err)
	}
	countQuery := `SELECT COUNT(*) FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=$1 AND i.status NOT IN ('completed','cancelled','failed') AND d.definition_json->>'owner_module_id'=$2`
	mock.ExpectQuery(regexp.QuoteMeta(countQuery)).WithArgs("tenant-a", "finance.ar").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	pinnedQuery := `SELECT COUNT(*) FROM _kora_workflow_attempt WHERE tenant_id=$1 AND status IN ('running','outcome_unknown') AND pinned_dispatch_json IS NOT NULL AND (pinned_dispatch_json->'dispatch'->>'caller_module_id'=$2 OR pinned_dispatch_json->'dispatch'->>'provider_module_id'=$3)`
	mock.ExpectQuery(regexp.QuoteMeta(pinnedQuery)).WithArgs("tenant-a", "finance.ar", "finance.ar").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	unknownQuery := `SELECT COUNT(*) FROM _kora_workflow_attempt a JOIN _kora_workflow_instance i ON i.tenant_id=a.tenant_id AND i.id=a.instance_id JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE a.tenant_id=$1 AND a.status='outcome_unknown' AND d.definition_json->>'owner_module_id'=$2`
	mock.ExpectQuery(regexp.QuoteMeta(unknownQuery)).WithArgs("tenant-a", "finance.ar").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	blockers, err := store.InspectOpenWork(context.Background(), "tenant-a", "finance.ar", 5)
	if err != nil || len(blockers) != 0 {
		t.Fatalf("postgres workflow blockers = %+v, %v", blockers, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistedWorkflowBusinessContextRetainsCausationDepth(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	raw, err := json.Marshal(persistedWorkflowBusinessContext{BusinessScope: scope, CausationDepth: 7})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := decodePersistedWorkflowBusinessContext(string(raw), "tenant-a")
	if err != nil || stored.BusinessScope != scope || stored.CausationDepth != 7 {
		t.Fatalf("workflow context round trip = %+v, %v", stored, err)
	}
	// Existing scope JSON remains readable and starts at the root depth.
	legacy, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	stored, err = decodePersistedWorkflowBusinessContext(string(legacy), "tenant-a")
	if err != nil || stored.BusinessScope != scope || stored.CausationDepth != 0 {
		t.Fatalf("legacy workflow context = %+v, %v", stored, err)
	}
}
