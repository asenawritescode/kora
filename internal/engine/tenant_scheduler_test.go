package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/scheduler"
)

type rejectingTenantWorkAdmission struct {
	acquired int
	released int
	err      error
}

func (a *rejectingTenantWorkAdmission) Acquire(context.Context) (func(), error) {
	a.acquired++
	if a.err != nil {
		return nil, a.err
	}
	return func() { a.released++ }, nil
}

func TestTenantSchedulersBindToTheirOwnDatabaseAndRegistry(t *testing.T) {
	dbA, mockA, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant A db:", err)
	}
	t.Cleanup(func() { _ = dbA.Close() })
	dbB, mockB, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant B db:", err)
	}
	t.Cleanup(func() { _ = dbB.Close() })
	registryA := doctype.NewRegistry()
	registryB := doctype.NewRegistry()
	bindingsA, bindingsB := &plugin.BindingPlan{Revision: 1}, &plugin.BindingPlan{Revision: 2}
	admissionA := &rejectingTenantWorkAdmission{}
	admissionB := &rejectingTenantWorkAdmission{}
	jobs := []*scheduler.JobConfig{
		{Name: "hourly", Type: scheduler.JobWebhook, Schedule: "@every 1h"},
		{Name: "application", Type: scheduler.JobType("application_task"), Schedule: "@every 1h"},
	}
	handlerFactory := ScheduledJobHandlerFactory(func(runtime TenantJobRuntime) map[scheduler.JobType]scheduler.JobHandler {
		if runtime.Database == nil || runtime.Registry == nil || runtime.SiteName == "" || runtime.TenantID == "" {
			t.Fatal("handler factory did not receive tenant-bound runtime resources")
		}
		if runtime.TenantID == "tenant-a" && runtime.ModuleBindings != bindingsA {
			t.Fatal("tenant A handler did not receive its own capability plan")
		}
		if runtime.TenantID == "tenant-b" && runtime.ModuleBindings != bindingsB {
			t.Fatal("tenant B handler did not receive its own capability plan")
		}
		return map[scheduler.JobType]scheduler.JobHandler{
			"application_task": func(context.Context, *scheduler.JobConfig) error { return nil },
		}
	})
	for _, mock := range []sqlmock.Sqlmock{mockA, mockB} {
		mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_schedule_definition").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_schedule_occurrence").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM information_schema\\.statistics").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	}

	schedulerA, err := newTenantScheduler(&net.LoadedSite{Name: "tenant-a", SiteID: "tenant-a", DB: dbA, Registry: registryA}, &kdb.MySQLDialect{}, jobs, nil, bindingsA, handlerFactory, nil, nil, nil, nil, nil, admissionA, nil)
	if err != nil {
		t.Fatal("construct tenant A scheduler:", err)
	}
	schedulerB, err := newTenantScheduler(&net.LoadedSite{Name: "tenant-b", SiteID: "tenant-b", DB: dbB, Registry: registryB}, &kdb.MySQLDialect{}, jobs, nil, bindingsB, handlerFactory, nil, nil, nil, nil, nil, admissionB, nil)
	if err != nil {
		t.Fatal("construct tenant B scheduler:", err)
	}
	if schedulerA == nil || schedulerB == nil {
		t.Fatal("tenant schedulers were not constructed")
	}
	if admissionA.acquired != 1 || admissionA.released != 1 || admissionB.acquired != 1 || admissionB.released != 1 {
		t.Fatalf("scheduler schema admissions A=%d/%d B=%d/%d, want one acquire/release per tenant", admissionA.acquired, admissionA.released, admissionB.acquired, admissionB.released)
	}
	if schedulerA.DB != dbA || schedulerA.Registry != registryA || schedulerA.TxManager.DB != dbA || schedulerA.TxManager.Registry != registryA || schedulerA.TxManager.SiteName != "tenant-a" {
		t.Fatal("tenant A scheduler is not bound to tenant A resources")
	}
	if schedulerB.DB != dbB || schedulerB.Registry != registryB || schedulerB.TxManager.DB != dbB || schedulerB.TxManager.Registry != registryB || schedulerB.TxManager.SiteName != "tenant-b" {
		t.Fatal("tenant B scheduler is not bound to tenant B resources")
	}
	if schedulerA.DB == schedulerB.DB || schedulerA.Registry == schedulerB.Registry || schedulerA.TxManager == schedulerB.TxManager {
		t.Fatal("tenant schedulers unexpectedly share tenant-bound resources")
	}
	if !schedulerA.CanExecute("application_task") || !schedulerB.CanExecute("application_task") {
		t.Fatal("application scheduled job handler was not registered for both tenants")
	}
	if schedulerA.DurableStore == nil || schedulerB.DurableStore == nil || schedulerA.TenantID != "tenant-a" || schedulerB.TenantID != "tenant-b" || schedulerA.WorkerID == schedulerB.WorkerID {
		t.Fatal("tenant schedulers are missing durable, tenant-scoped worker identity")
	}
	if err := mockA.ExpectationsWereMet(); err != nil {
		t.Fatal("tenant A scheduler store schema:", err)
	}
	if err := mockB.ExpectationsWereMet(); err != nil {
		t.Fatal("tenant B scheduler store schema:", err)
	}
}

func TestTenantSchedulerSchemaEnsureRequiresAdmission(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant database:", err)
	}
	defer database.Close()
	capacityErr := errors.New("tenant work capacity exhausted")
	admission := &rejectingTenantWorkAdmission{err: capacityErr}
	_, err = newTenantScheduler(
		&net.LoadedSite{Name: "tenant-a", SiteID: "tenant-a", DB: database, Registry: doctype.NewRegistry()},
		&kdb.MySQLDialect{}, []*scheduler.JobConfig{{Name: "hourly", Type: scheduler.JobWebhook, Schedule: "@every 1h"}},
		nil, nil, nil, nil, nil, nil, nil, nil, admission, nil,
	)
	if !errors.Is(err, capacityErr) {
		t.Fatalf("scheduler construction error = %v, want admission rejection", err)
	}
	if admission.acquired != 1 || admission.released != 0 {
		t.Fatalf("schema admission acquired/released = %d/%d, want 1/0", admission.acquired, admission.released)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scheduler schema DDL reached database without admission: %v", err)
	}
}

func TestTenantSchedulerSchemaIsPreparedWithoutConfiguredJobs(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant database:", err)
	}
	defer database.Close()
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_schedule_definition").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_schedule_occurrence").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM information_schema\\.statistics").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	worker, err := newTenantScheduler(
		&net.LoadedSite{Name: "tenant-a", SiteID: "tenant-a", DB: database, Registry: doctype.NewRegistry()},
		&kdb.MySQLDialect{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatal("prepare scheduler schema without jobs:", err)
	}
	if worker != nil {
		t.Fatal("scheduler worker was created without configured jobs")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("scheduler schema was not prepared for module inspection:", err)
	}
}
