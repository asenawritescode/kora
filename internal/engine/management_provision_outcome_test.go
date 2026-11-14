package engine

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/site"
)

// A stopped provision must say which step stopped on the operation, keep the raw
// error (hosts, driver text) off it, and put the full error in the Engine log
// with the operation and tenant.
func TestManagementProvisionReportsTheStepThatStopped(t *testing.T) {
	dial := errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")
	tests := []struct {
		name      string
		directory error
		create    error
		activate  error
		wantStep  string
	}{
		{name: "database unreachable", directory: sql.ErrNoRows, create: fmt.Errorf("connecting to database: pinging database: %w", dial), wantStep: "create tenant: connecting to database"},
		{name: "bootstrap fails", directory: sql.ErrNoRows, create: fmt.Errorf("bootstrapping system tables: %w", dial), wantStep: "create tenant: bootstrapping system tables"},
		{name: "unnamed create error", directory: sql.ErrNoRows, create: dial, wantStep: "create tenant"},
		{name: "directory unreadable", directory: dial, wantStep: "read tenant directory"},
		{name: "activation fails", directory: sql.ErrNoRows, activate: dial, wantStep: "activate tenant"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			command := validProvisionCommand()
			executor, err := newManagementProvisionExecutor(managementProvisionOptions{allocationRef: "mysql:shared-1", deploymentID: "deployment-1", platformDB: new(sql.DB), platformType: "mysql", common: &site.CommonConfig{DBType: "mysql"},
				admission: &managementProvisionAdmissionStub{}, directory: &managementProvisionDirectoryStub{err: tt.directory}, coordinator: &managementStateCoordinatorStub{err: tt.activate},
				createSite: func(_ context.Context, input site.CreateSiteInput) (*site.CreateSiteResult, error) {
					if tt.create != nil {
						return nil, tt.create
					}
					config := input.ResolvedTenantConfig()
					config.DomainsList = append([]string{input.Hostname}, input.ExtraDomains...)
					config.FileStorage = site.DefaultFileStorageFromEnv()
					if config.FileStorage == "s3" {
						config.StorageBucket = site.BucketNameForSite(input.Hostname)
					}
					return &site.CreateSiteResult{SiteID: input.SiteID, Config: config}, nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), managementTestClaim(provisionOperation(command)))
			if err != nil || result.State != contract.OperationRecoveryRequired || result.Error == nil {
				t.Fatalf("Execute = %+v, %v; want recovery_required with an error", result, err)
			}
			if !strings.Contains(result.Error.Message, `step "`+tt.wantStep+`"`) {
				t.Fatalf("operation message = %q, want step %q", result.Error.Message, tt.wantStep)
			}
			if strings.Contains(result.Error.Message, "127.0.0.1") {
				t.Fatalf("operation message leaks the raw error: %q", result.Error.Message)
			}
			line := logs.String()
			for _, want := range []string{"tenant provisioning stopped", "operation_id=operation-1", "tenant_id=tenant-a", "127.0.0.1:3306"} {
				if !strings.Contains(line, want) {
					t.Fatalf("log = %q, want %q", line, want)
				}
			}
		})
	}
}

// A definite refusal is logged too, so its time is in the Engine log.
func TestManagementProvisionLogsRefusals(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	command := validProvisionCommand()
	command.DatabaseAllocationRef = "mysql:other"
	executor, err := newManagementProvisionExecutor(managementProvisionOptions{allocationRef: "mysql:shared-1", deploymentID: "deployment-1", platformDB: new(sql.DB), platformType: "mysql", common: &site.CommonConfig{DBType: "mysql"},
		admission: &managementProvisionAdmissionStub{}, directory: &managementProvisionDirectoryStub{err: sql.ErrNoRows}, coordinator: &managementStateCoordinatorStub{},
		createSite: func(context.Context, site.CreateSiteInput) (*site.CreateSiteResult, error) {
			return nil, errors.New("unused")
		}})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := executor.Execute(context.Background(), managementTestClaim(provisionOperation(command)))
	if result.State != contract.OperationFailed || !strings.Contains(logs.String(), "tenant provisioning refused") || !strings.Contains(logs.String(), "operation_id=operation-1") {
		t.Fatalf("result = %+v, log = %q", result, logs.String())
	}
}
