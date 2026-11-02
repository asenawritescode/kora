package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
)

func TestScheduledJobCannotUseKernelGenericMutationOnCommandOnlyResource(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create scheduler database mock:", err)
	}
	defer database.Close()
	tenant := contract.TenantID("tenant-a")
	registry := doctype.NewRegistry()
	protected := &doctype.DocType{Name: "Protected Invoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands)}
	registry.Register(protected)
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: protected.Name, Role: doctype.AdminRole, Create: true, Read: true, Write: true, Delete: true})
	execution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{
		PrincipalID: "administrator", PrincipalType: contract.PrincipalHuman, Site: string(tenant),
		Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("create broad verified worker identity:", err)
	}
	operationKernel := kernel.New(kdb.Resolve("mysql"), nil)
	s := New(database, registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("mysql")}, nil)
	s.TenantID = string(tenant)
	var commandError *contract.Error
	if err := s.RegisterHandler("protected_write", func(ctx context.Context, _ *JobConfig) error {
		payload, _ := json.Marshal(kernel.RecordCreatePayload{Doctype: protected.Name, Data: json.RawMessage(`{"title":"must be denied"}`)})
		_, commandError = operationKernel.Execute(ctx, database, registry, kernel.Operation{
			Command: kernel.CommandRecordCreate, Payload: payload,
			Context: kernel.OperationContext{
				Site: string(tenant), TenantID: tenant,
				BusinessScope: contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant},
				Execution:     execution, Actor: execution.Actor(), User: "administrator", UserRole: doctype.AdminRole,
				Roles: []string{doctype.AdminRole}, Source: kernel.SourceWorker,
			},
		})
		return commandError
	}); err != nil {
		t.Fatal("register scheduled handler:", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO _kora_operation_audit").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	err = s.executeScheduledJob(context.Background(), &JobConfig{Name: "protected-write", Type: "protected_write"})
	if err == nil || commandError == nil || commandError.Type != contract.CodePermissionDenied {
		t.Fatalf("scheduled generic write error=%v kernel error=%v, want permission denied", err, commandError)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("scheduler performed an unexpected business database operation:", err)
	}
}
