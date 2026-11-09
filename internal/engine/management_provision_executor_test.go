package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/site"
)

type managementProvisionAdmissionStub struct {
	acquired int
	released int
	err      error
}

func (a *managementProvisionAdmissionStub) Acquire(context.Context) (func(), error) {
	a.acquired++
	if a.err != nil {
		return nil, a.err
	}
	return func() { a.released++ }, nil
}

type managementProvisionDirectoryStub struct {
	info site.DBSiteInfo
	err  error
}

func (d *managementProvisionDirectoryStub) GetByIDContext(context.Context, string) (site.DBSiteInfo, error) {
	return d.info, d.err
}

func TestManagementProvisionExecutorCreatesCanonicalTenantAndActivates(t *testing.T) {
	command := validProvisionCommand()
	common := &site.CommonConfig{DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBUser: "engine"}
	admission := &managementProvisionAdmissionStub{}
	directory := &managementProvisionDirectoryStub{err: sql.ErrNoRows}
	coordinator := &managementStateCoordinatorStub{}
	createCalls := 0
	executor, err := newManagementProvisionExecutor("mysql:shared-1", "deployment-1", new(sql.DB), "mysql", common, admission, directory, coordinator, func(_ context.Context, input site.CreateSiteInput) (*site.CreateSiteResult, error) {
		createCalls++
		if input.SiteID != string(command.Tenant) || input.Hostname != command.PrimaryAlias || input.AdminEmail != command.BootstrapAdminEmail {
			t.Fatalf("CreateSite identity/bootstrap = %q/%q/%q", input.SiteID, input.Hostname, input.AdminEmail)
		}
		if input.RuntimeCellID != "deployment-1" {
			t.Fatalf("CreateSite deployment assignment = %q, want %q", input.RuntimeCellID, "deployment-1")
		}
		if len(input.ExtraDomains) != len(command.Aliases) || input.DBHost != common.DBHost || input.DBUser != common.DBUser {
			t.Fatalf("CreateSite aliases/database = %#v/%q/%q", input.ExtraDomains, input.DBHost, input.DBUser)
		}
		config := input.ResolvedTenantConfig()
		config.DomainsList = append([]string{input.Hostname}, input.ExtraDomains...)
		config.FileStorage = site.DefaultFileStorageFromEnv()
		if config.FileStorage == "s3" {
			config.StorageBucket = site.BucketNameForSite(input.Hostname)
		}
		return &site.CreateSiteResult{SiteID: string(command.Tenant), Config: config}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), managementTestClaim(provisionOperation(command)))
	if err != nil || result.State != contract.OperationSucceeded {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	if createCalls != 1 || coordinator.activated != string(command.Tenant) || admission.acquired != 1 || admission.released != 1 {
		t.Fatalf("create/activate/admission = %d/%q/%d/%d", createCalls, coordinator.activated, admission.acquired, admission.released)
	}
}

func TestManagementProvisionExecutorReplaysActiveTenantWithoutRecreating(t *testing.T) {
	command := validProvisionCommand()
	common := &site.CommonConfig{DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBUser: "engine"}
	admission := &managementProvisionAdmissionStub{}
	coordinator := &managementStateCoordinatorStub{}
	executor, err := newManagementProvisionExecutor("mysql:shared-1", "deployment-1", new(sql.DB), "mysql", common, admission, nil, coordinator, nil)
	if err == nil {
		t.Fatal("constructor accepted missing dependencies")
	}
	directory := &managementProvisionDirectoryStub{}
	executor, err = newManagementProvisionExecutor("mysql:shared-1", "deployment-1", new(sql.DB), "mysql", common, admission, directory, coordinator, func(context.Context, site.CreateSiteInput) (*site.CreateSiteResult, error) {
		t.Fatal("active idempotent replay recreated tenant data")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	directory.info = executor.expectedSiteInfo(command)
	directory.info.Status = "active"
	result, err := executor.Execute(context.Background(), managementTestClaim(provisionOperation(command)))
	if err != nil || result.State != contract.OperationSucceeded {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	if coordinator.activated != string(command.Tenant) || admission.acquired != 1 || admission.released != 1 {
		t.Fatalf("activate/admission = %q/%d/%d", coordinator.activated, admission.acquired, admission.released)
	}
}

func TestManagementProvisionExecutorRetainsLegacyDefaultAndRejectsExplicitMisroute(t *testing.T) {
	for _, test := range []struct {
		name       string
		assignment string
		want       contract.OperationState
		wantCode   contract.Code
	}{
		{name: "legacy blank default remains compatible", assignment: "", want: contract.OperationSucceeded},
		{name: "explicit other deployment is rejected", assignment: "deployment-old", want: contract.OperationFailed, wantCode: contract.CodeConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := validProvisionCommand()
			common := &site.CommonConfig{DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBUser: "engine"}
			admission := &managementProvisionAdmissionStub{}
			coordinator := &managementStateCoordinatorStub{}
			directory := &managementProvisionDirectoryStub{}
			executor, err := newManagementProvisionExecutor("mysql:shared-1", "deployment-1", new(sql.DB), "mysql", common, admission, directory, coordinator, func(context.Context, site.CreateSiteInput) (*site.CreateSiteResult, error) {
				t.Fatal("active replay recreated tenant data")
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			directory.info = executor.expectedSiteInfo(command)
			directory.info.RuntimeCellID = test.assignment
			directory.info.Status = "active"
			result, err := executor.Execute(context.Background(), managementTestClaim(provisionOperation(command)))
			if err != nil || result.State != test.want {
				t.Fatalf("Execute = %+v, %v; want %s", result, err, test.want)
			}
			if test.wantCode != "" && (result.Error == nil || result.Error.Type != test.wantCode) {
				t.Fatalf("Execute error = %+v; want code %s", result.Error, test.wantCode)
			}
			if test.want == contract.OperationSucceeded && coordinator.activated != string(command.Tenant) {
				t.Fatalf("legacy default replay activated %q; want %q", coordinator.activated, command.Tenant)
			}
			if test.want == contract.OperationFailed && coordinator.activated != "" {
				t.Fatalf("misrouted replay activated tenant %q", coordinator.activated)
			}
		})
	}
}

func TestManagementProvisionExecutorRejectsUnassignedAndPackagedCommandsBeforeSideEffects(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*contract.ProvisionTenant)
		want   contract.Code
	}{
		{name: "allocation", change: func(cmd *contract.ProvisionTenant) { cmd.DatabaseAllocationRef = "mysql:other" }, want: contract.CodeConflict},
		{name: "deployment", change: func(cmd *contract.ProvisionTenant) { cmd.Deployment = "deployment-2" }, want: contract.CodeConflict},
		{name: "package digest", change: func(cmd *contract.ProvisionTenant) { cmd.PackageDigest = strings.Repeat("a", 64) }, want: contract.CodeUnsupportedVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := validProvisionCommand()
			test.change(&command)
			admission := &managementProvisionAdmissionStub{}
			coordinator := &managementStateCoordinatorStub{}
			directory := &managementProvisionDirectoryStub{err: sql.ErrNoRows}
			executor, err := newManagementProvisionExecutor("mysql:shared-1", "deployment-1", new(sql.DB), "mysql", &site.CommonConfig{DBType: "mysql"}, admission, directory, coordinator, func(context.Context, site.CreateSiteInput) (*site.CreateSiteResult, error) {
				t.Fatal("invalid command reached tenant creation")
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), managementTestClaim(provisionOperation(command)))
			if err != nil || result.State != contract.OperationFailed || result.Error == nil || result.Error.Type != test.want {
				t.Fatalf("Execute = %+v, %v; want code %s", result, err, test.want)
			}
			if admission.acquired != 0 || coordinator.activated != "" {
				t.Fatalf("invalid command caused admission/runtime effects: %d/%q", admission.acquired, coordinator.activated)
			}
		})
	}
}

func TestManagementProvisionExecutorRejectsUnresolvedLibSQLAllocation(t *testing.T) {
	_, err := newManagementProvisionExecutor(
		"libsql:shared-1", "deployment-1", new(sql.DB), "libsql",
		&site.CommonConfig{DBType: "libsql"}, &managementProvisionAdmissionStub{},
		&managementProvisionDirectoryStub{err: sql.ErrNoRows}, &managementStateCoordinatorStub{},
		func(context.Context, site.CreateSiteInput) (*site.CreateSiteResult, error) { return nil, nil },
	)
	if err == nil || !strings.Contains(err.Error(), "matching MySQL or PostgreSQL") {
		t.Fatalf("unresolved LibSQL allocation error = %v", err)
	}
}

func TestManagementProvisionExecutorMakesUncertainCreateRecoverable(t *testing.T) {
	command := validProvisionCommand()
	admission := &managementProvisionAdmissionStub{}
	coordinator := &managementStateCoordinatorStub{}
	executor, err := newManagementProvisionExecutor("mysql:shared-1", "deployment-1", new(sql.DB), "mysql", &site.CommonConfig{DBType: "mysql"}, admission, &managementProvisionDirectoryStub{err: sql.ErrNoRows}, coordinator, func(context.Context, site.CreateSiteInput) (*site.CreateSiteResult, error) {
		return nil, errors.New("database connection interrupted after create")
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), managementTestClaim(provisionOperation(command)))
	if err != nil || result.State != contract.OperationRecoveryRequired {
		t.Fatalf("Execute = %+v, %v; want recoverable outcome", result, err)
	}
	if result.Error == nil || result.Error.Type != contract.CodeDependencyUnavailable || coordinator.activated != "" || admission.released != 1 {
		t.Fatalf("unexpected uncertain create outcome: %+v coordinator=%q released=%d", result, coordinator.activated, admission.released)
	}
}

func validProvisionCommand() contract.ProvisionTenant {
	return contract.ProvisionTenant{
		Meta: contract.CommandMeta{IdempotencyKey: "provision-operation-1"}, Tenant: "tenant-a",
		BootstrapAdminEmail: "admin@example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "acme.example.test", Aliases: []string{"www.acme.example.test"},
		Deployment: "deployment-1", DatabaseAllocationRef: "mysql:shared-1",
	}
}

func provisionOperation(command contract.ProvisionTenant) management.Operation {
	payload, _ := json.Marshal(command)
	return management.Operation{ID: "operation-1", Tenant: command.Tenant, Kind: management.CommandProvisionTenant, Command: payload}
}
