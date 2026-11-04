package engine

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/tenantruntime"
)

func TestScheduledModuleInspectionEnsuresSchemaUnderTenantAdmission(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create database mock:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_schedule_definition").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_schedule_occurrence").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM information_schema\\.statistics").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT job_name,job_json FROM _kora_schedule_definition").WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"job_name", "job_json"}))

	admission, err := dbaccess.NewAdmission(1, 0, time.Second)
	if err != nil {
		t.Fatal("create tenant work admission:", err)
	}
	runtime := &tenantruntime.Snapshot{
		Activation: tenantruntime.Activation{Tenant: "tenant-a"},
		DBType:     "mysql",
		DB:         database,
		Services:   tenantruntime.Services{WorkAdmission: admission},
	}
	blockers, err := inspectScheduledModuleWork(context.Background(), runtime, "tenant-a", contract.ModuleID("finance.ar"), 10)
	if err != nil {
		t.Fatal("inspect scheduled module work:", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("empty scheduled work returned blockers: %+v", blockers)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}
