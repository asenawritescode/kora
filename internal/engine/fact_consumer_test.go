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
		{name: "tenant without a Facts doctype registers nothing", withFacts: false, wantStarts: 0},
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
			sale := &doctype.DocType{Name: "Sale"}
			if tc.withFacts {
				sale.Facts = &doctype.FactModel{Time: "creation"}
			}
			reg.Register(sale)
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

// TestFactConsumerStartsDriftCheck checks that starting the fact workers starts the drift check, which is the
// first statement to touch its lease table. The lease DDL fails on purpose, so the check stops after one call.
func TestFactConsumerStartsDriftCheck(t *testing.T) {
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
	deadline := time.After(5 * time.Second)
	for mock.ExpectationsWereMet() != nil {
		select {
		case <-deadline:
			t.Fatalf("drift check did not touch its lease table: %v", mock.ExpectationsWereMet())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
