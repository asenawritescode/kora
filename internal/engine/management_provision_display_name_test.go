package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/site"
)

func TestManagementProvisionExecutorPassesNormalizedDisplayName(t *testing.T) {
	tests := []struct {
		name, displayName, want string
	}{
		{name: "legacy command without display name", displayName: "", want: ""},
		{name: "typed business name", displayName: "Dbg Duka", want: "Dbg Duka"},
		{name: "padded business name is trimmed", displayName: "  Mama Njeri Duka  ", want: "Mama Njeri Duka"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := validProvisionCommand()
			command.DisplayName = test.displayName
			common := &site.CommonConfig{DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBUser: "engine"}
			var got *string
			executor, err := newManagementProvisionExecutor(managementProvisionOptions{allocationRef: "mysql:shared-1", deploymentID: "deployment-1", platformDB: new(sql.DB), platformType: "mysql", common: common, admission: &managementProvisionAdmissionStub{}, directory: &managementProvisionDirectoryStub{err: sql.ErrNoRows}, coordinator: &managementStateCoordinatorStub{}, createSite: func(_ context.Context, input site.CreateSiteInput) (*site.CreateSiteResult, error) {
				value := input.DisplayName
				got = &value
				config := input.ResolvedTenantConfig()
				config.DomainsList = append([]string{input.Hostname}, input.ExtraDomains...)
				config.FileStorage = site.DefaultFileStorageFromEnv()
				if config.FileStorage == "s3" {
					config.StorageBucket = site.BucketNameForSite(input.Hostname)
				}
				return &site.CreateSiteResult{SiteID: string(command.Tenant), Config: config}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), managementTestClaim(provisionOperation(command)))
			if err != nil || result.State != contract.OperationSucceeded {
				t.Fatalf("Execute = %+v, %v", result, err)
			}
			if got == nil || *got != test.want {
				t.Fatalf("CreateSite display name = %v, want %q", got, test.want)
			}
		})
	}
}
