package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/site"
)

type managementProvisionDirectory interface {
	GetByIDContext(context.Context, string) (site.DBSiteInfo, error)
}

type managementCreateSite func(context.Context, site.CreateSiteInput) (*site.CreateSiteResult, error)

type managementProvisionExecutor struct {
	allocationRef string
	deployment    contract.DeploymentID
	platformDB    *sql.DB
	platformType  string
	common        *site.CommonConfig
	admission     managementWorkAdmission
	directory     managementProvisionDirectory
	coordinator   managementStateCoordinator
	createSite    managementCreateSite
}

// managementProvisionOptions are the dependencies of the tenant provisioning
// executor; every field is required.
type managementProvisionOptions struct {
	allocationRef string
	deploymentID  string
	platformDB    *sql.DB
	platformType  string
	common        *site.CommonConfig
	admission     managementWorkAdmission
	directory     managementProvisionDirectory
	coordinator   managementStateCoordinator
	createSite    managementCreateSite
}

func newManagementProvisionExecutor(opts managementProvisionOptions) (*managementProvisionExecutor, error) {
	deployment, err := contract.ParseDeploymentID(opts.deploymentID)
	if err != nil || opts.allocationRef == "" || len(opts.allocationRef) > 160 || opts.platformDB == nil || opts.platformType == "" || opts.common == nil || opts.common.DBType == "" || opts.admission == nil || opts.directory == nil || opts.coordinator == nil || opts.createSite == nil {
		return nil, errors.New("management provisioning requires a deployment, allowlisted allocation, platform database, tenant database config, admission, directory, coordinator, and creator")
	}
	if !strings.EqualFold(opts.platformType, opts.common.DBType) || (strings.ToLower(opts.common.DBType) != "mysql" && strings.ToLower(opts.common.DBType) != "postgres") {
		return nil, errors.New("static management database allocation currently requires matching MySQL or PostgreSQL platform and tenant databases")
	}
	if err := site.ValidateTenantDatabaseName(opts.common.DBType, "management_probe"); err != nil {
		return nil, fmt.Errorf("management provisioning database allocation uses an unsupported tenant dialect: %w", err)
	}
	if err := site.ValidateTenantDatabaseName(opts.platformType, "management_probe"); err != nil {
		return nil, fmt.Errorf("management provisioning platform database uses an unsupported dialect: %w", err)
	}
	return &managementProvisionExecutor{
		allocationRef: opts.allocationRef,
		deployment:    deployment,
		platformDB:    opts.platformDB,
		platformType:  opts.platformType,
		common:        opts.common,
		admission:     opts.admission,
		directory:     opts.directory,
		coordinator:   opts.coordinator,
		createSite:    opts.createSite,
	}, nil
}

func (e *managementProvisionExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	failed := func(code contract.Code, message string) management.FinishResult {
		return refused(claim.Operation, code, message)
	}
	if e == nil || e.directory == nil || e.coordinator == nil || e.createSite == nil {
		return management.FinishResult{}, errors.New("management provisioning executor is not configured")
	}
	operation := claim.Operation
	if claim.Owner == "" || claim.Fence == 0 {
		return management.FinishResult{}, errors.New("management provisioning executor requires a fenced claim")
	}
	if operation.Kind != management.CommandProvisionTenant {
		return failed(contract.CodeValidationFailed, "operation kind is not enabled on this Engine deployment"), nil
	}
	var command contract.ProvisionTenant
	if err := json.Unmarshal(operation.Command, &command); err != nil {
		return failed(contract.CodeValidationFailed, "stored tenant provisioning command is invalid"), nil
	}
	if err := contract.ValidateProvisionTenant(command); err != nil {
		return failed(contract.CodeValidationFailed, "stored tenant provisioning command failed validation"), nil
	}
	if command.Tenant != operation.Tenant {
		return failed(contract.CodeValidationFailed, "stored tenant provisioning command does not match its operation"), nil
	}
	if command.Deployment != e.deployment || command.DatabaseAllocationRef != e.allocationRef {
		return failed(contract.CodeConflict, "tenant provisioning command targets an unassigned deployment or database allocation"), nil
	}
	if command.PackageDigest != "" {
		return failed(contract.CodeUnsupportedVersion, "package application is not enabled on this Engine deployment"), nil
	}
	expectedAssignment, err := site.NormalizeRuntimeCellID(string(command.Deployment))
	if err != nil || expectedAssignment == "" {
		return failed(contract.CodeValidationFailed, "tenant deployment cannot be represented by the legacy assignment adapter"), nil
	}
	release, err := e.admission.Acquire(ctx)
	if err != nil {
		return stepAdmission.stopped(operation, err), nil
	}
	defer func() {
		if release != nil {
			release()
		}
	}()

	info, err := e.directory.GetByIDContext(ctx, string(command.Tenant))
	if err == nil {
		if info.SiteID != string(command.Tenant) || info.Name != command.PrimaryAlias || !sameAliases(info.Domains, append([]string{command.PrimaryAlias}, command.Aliases...)) || site.DataPlaneReference(info) != site.DataPlaneReference(e.expectedSiteInfo(command)) {
			return failed(contract.CodeConflict, "existing tenant identity, aliases, or database allocation does not match this provisioning command"), nil
		}
		actualAssignment, actualErr := site.NormalizeRuntimeCellID(info.RuntimeCellID)
		// Blank assignments are retained as the v1 default compatibility path
		// for existing active tenants. Only an explicit stored assignment can
		// prove this management command is routed to the wrong deployment.
		if actualErr != nil || (actualAssignment != "" && actualAssignment != expectedAssignment) {
			return failed(contract.CodeConflict, "existing tenant deployment assignment does not match this provisioning command"), nil
		}
		if info.Status != "active" && info.Status != "provisioning" {
			return failed(contract.CodeConflict, "existing tenant lifecycle state cannot be resumed by provisioning"), nil
		}
		if info.Status == "provisioning" {
			if err := e.createTenant(ctx, command); err != nil {
				return stepCreateTenant.stopped(operation, err), nil
			}
		}
		release()
		release = nil
		if err := e.coordinator.ActivateFromDirectory(ctx, string(command.Tenant)); err != nil {
			return stepActivate.stopped(operation, err), nil
		}
		return management.FinishResult{State: contract.OperationSucceeded}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return stepReadDirectory.stopped(operation, err), nil
	}
	if err := e.createTenant(ctx, command); err != nil {
		return stepCreateTenant.stopped(operation, err), nil
	}
	release()
	release = nil
	if err := e.coordinator.ActivateFromDirectory(ctx, string(command.Tenant)); err != nil {
		return stepActivate.stopped(operation, err), nil
	}
	return management.FinishResult{State: contract.OperationSucceeded}, nil
}

func (e *managementProvisionExecutor) createTenant(ctx context.Context, command contract.ProvisionTenant) error {
	assignment, err := site.NormalizeRuntimeCellID(string(command.Deployment))
	if err != nil || assignment == "" {
		return errors.New("tenant deployment cannot be represented by the legacy assignment adapter")
	}
	displayName, err := contract.NormalizeTenantDisplayName(command.DisplayName)
	if err != nil {
		return fmt.Errorf("tenant display name: %w", err)
	}
	input := site.CreateSiteInput{
		Hostname: command.PrimaryAlias, SiteID: string(command.Tenant), AdminEmail: command.BootstrapAdminEmail, CloudSubjectID: command.BootstrapAdminSubject,
		AdminFullName: "Administrator", ExtraDomains: append([]string(nil), command.Aliases...),
		RuntimeCellID: assignment, DisplayName: displayName,
		DBType: e.common.DBType, DBHost: e.common.DBHost, DBPort: e.common.DBPort,
		DBUser: e.common.DBUser, DBPassword: e.common.DBPassword,
		PlatformDBType: e.platformType, PlatformDBHost: e.common.DBHost, PlatformDBPort: e.common.DBPort,
		PlatformDBUser: e.common.DBUser, PlatformDBPassword: e.common.DBPassword, PlatformDB: e.platformDB,
	}
	created, err := e.createSite(ctx, input)
	if err != nil {
		return fmt.Errorf("create tenant: %w", err)
	}
	if created == nil || created.Config == nil || created.SiteID != string(command.Tenant) || created.Config.Hostname != command.PrimaryAlias {
		if created != nil && created.DB != nil && created.DB != e.platformDB {
			_ = created.DB.Close()
		}
		return errors.New("tenant creator returned a mismatched identity or incomplete result")
	}
	createdInfo := site.DBSiteInfo{
		SiteID: created.SiteID, Name: created.Config.Hostname, Domains: created.Config.Domains(),
		RuntimeCellID: assignment,
		DBType:        created.Config.DBType, DBHost: created.Config.DBHost, DBPort: created.Config.DBPort,
		DBName: created.Config.DBName, DBUser: created.Config.DBUser,
		FileStorage: created.Config.FileStorage, StorageBucket: created.Config.StorageBucket,
	}
	if !sameAliases(createdInfo.Domains, append([]string{command.PrimaryAlias}, command.Aliases...)) || site.DataPlaneReference(createdInfo) != site.DataPlaneReference(e.expectedSiteInfo(command)) {
		if created.DB != nil && created.DB != e.platformDB {
			_ = created.DB.Close()
		}
		return errors.New("tenant creator returned a mismatched alias set or database allocation")
	}
	if created.DB != nil && created.DB != e.platformDB {
		if err := created.DB.Close(); err != nil {
			return fmt.Errorf("close tenant bootstrap connection: %w", err)
		}
	}
	return nil
}

func (e *managementProvisionExecutor) expectedSiteInfo(command contract.ProvisionTenant) site.DBSiteInfo {
	input := site.CreateSiteInput{
		Hostname: command.PrimaryAlias, DBType: e.common.DBType, DBHost: e.common.DBHost, DBPort: e.common.DBPort,
		DBUser: e.common.DBUser, DBPassword: e.common.DBPassword,
		PlatformDBType: e.platformType, PlatformDBHost: e.common.DBHost, PlatformDBPort: e.common.DBPort,
		PlatformDBUser: e.common.DBUser, PlatformDBPassword: e.common.DBPassword,
	}
	cfg := input.ResolvedTenantConfig()
	fileStorage := site.DefaultFileStorageFromEnv()
	info := site.DBSiteInfo{
		SiteID: string(command.Tenant), Name: command.PrimaryAlias,
		RuntimeCellID: strings.ToLower(strings.TrimSpace(string(command.Deployment))),
		Domains:       append([]string{command.PrimaryAlias}, command.Aliases...),
		DBType:        cfg.DBType, DBHost: cfg.DBHost, DBPort: cfg.DBPort, DBName: cfg.DBName,
		DBUser: cfg.DBUser, FileStorage: fileStorage,
	}
	if fileStorage == "s3" {
		info.StorageBucket = site.BucketNameForSite(command.PrimaryAlias)
	}
	return info
}

func sameAliases(left, right []string) bool {
	canonical := func(values []string) []string {
		result := make([]string, 0, len(values))
		for _, value := range values {
			result = append(result, strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), ".")))
		}
		sort.Strings(result)
		return result
	}
	a, b := canonical(left), canonical(right)
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

var _ management.OperationExecutor = (*managementProvisionExecutor)(nil)
