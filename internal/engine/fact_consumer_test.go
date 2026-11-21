package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/tenantruntime"
)

// TestRecordConsumersRegisterFactConsumerOnlyWithFactDoctypes checks that the fact
// consumer's tenant worker is registered for a tenant with a Facts doctype and not otherwise.
func TestRecordConsumersRegisterFactConsumerOnlyWithFactDoctypes(t *testing.T) {
	tests := []struct {
		name       string
		withFacts  bool
		wantStarts int
	}{
		{name: "tenant with a Facts doctype registers the consumer", withFacts: true, wantStarts: 1},
		{name: "tenant with only child tables registers nothing", withFacts: false, wantStarts: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database, _, err := sqlmock.New()
			if err != nil {
				t.Fatal("create tenant DB:", err)
			}
			defer database.Close()
			admission, err := dbaccess.NewAdmission(1, 1, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			reg := doctype.NewRegistry()
			// A record type gets a generated fact model unless it is a child table, so only a child
			// table leaves a tenant with nothing to register.
			record := &doctype.DocType{Name: "Line", IsChildTable: true}
			if tc.withFacts {
				record = &doctype.DocType{Name: "Sale", Facts: &doctype.FactModel{Time: "creation"}}
			}
			reg.Register(record)
			loaded := &knet.LoadedSite{SiteID: "tenant-facts", DBType: "mysql", DB: database, Registry: reg}
			starts := 0
			config := TenantServiceConfig{
				ProcessContext: context.Background(),
				RegisterWorkerStart: func(func() error) error {
					starts++
					return nil
				},
			}
			services := &tenantruntime.Services{WorkAdmission: admission}
			if err := registerRecordConsumers(config, loaded, services, nil, kdb.Resolve("mysql")); err != nil {
				t.Fatalf("registerRecordConsumers: %v", err)
			}
			if starts != tc.wantStarts {
				t.Fatalf("worker starts = %d, want %d", starts, tc.wantStarts)
			}
		})
	}
}

// TestFactConsumerWaitsForSyncBeforeDriftCheck checks that the drift check does not touch the fact tables before the
// first model sync has succeeded. The sync cannot succeed here (its first statement is not expected), so the drift
// check's lease table must stay untouched.
func TestFactConsumerWaitsForSyncBeforeDriftCheck(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant DB:", err)
	}
	defer database.Close()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec(".*_kora_fact_drift_lease").WillReturnError(errors.New("drift lease probe"))
	admission, err := dbaccess.NewAdmissionWithMaintenance(2, 1, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale", Facts: &doctype.FactModel{Time: "creation"}})
	loaded := &knet.LoadedSite{SiteID: "tenant-drift", Name: "tenant-drift", DBType: "mysql", DB: database, Registry: reg}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := TenantServiceConfig{
		ProcessContext: ctx,
		RegisterWorkerStart: func(start func() error) error {
			return start()
		},
	}
	services := &tenantruntime.Services{WorkAdmission: admission}
	if err := registerRecordConsumers(config, loaded, services, nil, kdb.Resolve("mysql")); err != nil {
		t.Fatalf("registerRecordConsumers: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if mock.ExpectationsWereMet() == nil {
		t.Fatal("drift check touched its lease table before the fact model sync succeeded")
	}
}

// TestFactConsumerStartsWhenBackfillQueueFails checks that a failed backfill queue is logged and the fact
// workers still start: the tenant comes up and a later start retries the queue.
func TestFactConsumerStartsWhenBackfillQueueFails(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant DB:", err)
	}
	defer database.Close()
	mock.ExpectExec(".*_kora_fact_rebuild_job").WillReturnError(errors.New("backfill probe"))
	admission, err := dbaccess.NewAdmissionWithMaintenance(2, 1, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Expense", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	loaded := &knet.LoadedSite{SiteID: "tenant-backfill", Name: "backfill.example.test", DBType: "mysql", DB: database, Registry: reg}
	starts := 0
	config := TenantServiceConfig{
		ProcessContext:      context.Background(),
		RegisterWorkerStart: func(start func() error) error { starts++; return start() },
	}
	services := &tenantruntime.Services{WorkAdmission: admission}
	if err := registerRecordConsumers(config, loaded, services, nil, kdb.Resolve("mysql")); err != nil {
		t.Fatalf("registerRecordConsumers: %v; a failed backfill queue must not fail the start", err)
	}
	if starts != 1 {
		t.Fatalf("worker starts = %d, want 1", starts)
	}
}
