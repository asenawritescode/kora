package contract

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
)

//contract:enum:v2
type TenantState string

const (
	TenantProvisioning TenantState = "provisioning"
	TenantActive       TenantState = "active"
	TenantSuspended    TenantState = "suspended"
	TenantDeleting     TenantState = "deleting"
	TenantFailed       TenantState = "failed"
)

// ManagementCapability names a durable operation route that this Engine has
// composed with its worker and authorization boundary.
//
//contract:enum:v2
type ManagementCapability string

const (
	ManagementCapabilityTenantProvision       ManagementCapability = "tenant.provision"
	ManagementCapabilityTenantProvisionRetry  ManagementCapability = "tenant.provision.reconcile"
	ManagementCapabilityTenantPackageApply    ManagementCapability = "tenant.package.apply"
	ManagementCapabilityTenantPackageRetry    ManagementCapability = "tenant.package.reconcile"
	ManagementCapabilityTenantStateSet        ManagementCapability = "tenant.state.set"
	ManagementCapabilityTenantStateRetry      ManagementCapability = "tenant.state.reconcile"
	ManagementCapabilityTenantDelete          ManagementCapability = "tenant.delete"
	ManagementCapabilityTenantDeleteRetry     ManagementCapability = "tenant.delete.reconcile"
	ManagementCapabilityTenantInvitation      ManagementCapability = "tenant.invitation.deliver"
	ManagementCapabilityTenantInvitationRetry ManagementCapability = "tenant.invitation.reconcile"
	ManagementCapabilityOperationRead         ManagementCapability = "operation.read"
)

// ManagementCapabilitiesV2 is an authenticated, read-only projection of the
// management routes that the current Engine process has enabled.
//
//contract:wire:v2
type ManagementCapabilitiesV2 struct {
	Version      uint16                 `json:"version"`
	Capabilities []ManagementCapability `json:"capabilities"`
}

func ValidateManagementCapabilitiesV2(value ManagementCapabilitiesV2) error {
	if value.Version != 2 || len(value.Capabilities) == 0 || len(value.Capabilities) > 32 {
		return fmt.Errorf("management capabilities must use version 2 and contain 1 to 32 entries")
	}
	known := map[ManagementCapability]struct{}{
		ManagementCapabilityTenantProvision: {}, ManagementCapabilityTenantProvisionRetry: {},
		ManagementCapabilityTenantPackageApply: {}, ManagementCapabilityTenantPackageRetry: {},
		ManagementCapabilityTenantStateSet: {}, ManagementCapabilityTenantStateRetry: {},
		ManagementCapabilityTenantDelete: {}, ManagementCapabilityTenantDeleteRetry: {},
		ManagementCapabilityTenantInvitation:      {},
		ManagementCapabilityTenantInvitationRetry: {},
		ManagementCapabilityOperationRead:         {},
	}
	seen := make(map[ManagementCapability]struct{}, len(value.Capabilities))
	for _, capability := range value.Capabilities {
		if _, ok := known[capability]; !ok {
			return fmt.Errorf("unknown Engine management capability %q", capability)
		}
		if _, ok := seen[capability]; ok {
			return fmt.Errorf("duplicate Engine management capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	if _, ok := seen[ManagementCapabilityOperationRead]; !ok {
		return fmt.Errorf("management capabilities must include operation.read")
	}
	return nil
}

//contract:wire:v2
type ManagedOperation struct {
	ID               OperationID    `json:"id"`
	Tenant           TenantID       `json:"tenant_id"`
	State            OperationState `json:"state"`
	ObservedRevision Revision       `json:"observed_revision,omitempty"`
	Error            *Error         `json:"error,omitempty"`
}

//contract:wire:v2
type ProvisionTenant struct {
	Meta   CommandMeta `json:"meta"`
	Tenant TenantID    `json:"tenant_id"`
	// The Engine creates a passwordless bootstrap administrator. This command
	// does not claim or trigger enrollment-link delivery; that requires a
	// separately durable notification flow. A password never crosses the
	// management boundary.
	//contract:minLength=3
	//contract:maxLength=254
	BootstrapAdminEmail string `json:"bootstrap_admin_email"`
	// PrimaryAlias is the canonical hostname used for tenant application routes
	// and configuration records. Aliases contains only additional hostnames.
	//contract:minLength=1
	//contract:maxLength=253
	//contract:pattern=^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$
	PrimaryAlias string `json:"primary_alias"`
	//contract:maxItems=49
	//contract:itemPattern=^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$
	Aliases    []string     `json:"aliases"`
	Deployment DeploymentID `json:"deployment_id"`
	//contract:minLength=3
	//contract:maxLength=160
	//contract:pattern=^[A-Za-z0-9][A-Za-z0-9._:-]{1,159}$
	DatabaseAllocationRef string `json:"database_allocation_ref"`
	//contract:minLength=64
	//contract:maxLength=64
	//contract:pattern=^[a-f0-9]{64}$
	PackageDigest string `json:"package_digest,omitempty"`
}

// RequestTenantInvitation starts a durable Engine-owned sign-in-link
// delivery operation for the active tenant administrator. Tenant identity is
// supplied by the authenticated management route.
//
//contract:wire:v2
type RequestTenantInvitation struct {
	Meta CommandMeta `json:"meta"`
	//contract:minLength=3
	//contract:maxLength=254
	//contract:maxBytes=254
	Email string `json:"email"`
}

// InvitationDeliveryOutcome is an operator's verified provider finding after
// an SMTP result was uncertain. "not_delivered" authorizes a fresh link send.
//
//contract:enum:v2
type InvitationDeliveryOutcome string

const (
	InvitationDelivered    InvitationDeliveryOutcome = "delivered"
	InvitationNotDelivered InvitationDeliveryOutcome = "not_delivered"
)

// ReconcileTenantInvitationOperation resolves one uncertain invitation under
// the same durable operation identity.
//
//contract:wire:v2
type ReconcileTenantInvitationOperation struct {
	Meta    CommandMeta               `json:"meta"`
	Outcome InvitationDeliveryOutcome `json:"outcome"`
	//contract:minLength=8
	//contract:maxLength=1000
	Reason string `json:"reason"`
}

//contract:wire:v2
type ApplyPackage struct {
	Meta   CommandMeta `json:"meta"`
	Tenant TenantID    `json:"tenant_id"`
	//contract:minLength=3
	//contract:maxLength=160
	//contract:pattern=^[A-Za-z0-9][A-Za-z0-9._:-]{1,159}$
	ArtifactRef string `json:"artifact_ref"`
	//contract:minLength=64
	//contract:maxLength=64
	//contract:pattern=^[a-f0-9]{64}$
	Digest                 string   `json:"digest"`
	ExpectedConfigRevision Revision `json:"expected_config_revision"`
}

//contract:wire:v2
type TenantDescriptorV2 struct {
	//contract:const=2
	Version uint16   `json:"version"`
	SiteID  TenantID `json:"site_id"`
	//contract:maxItems=50
	//contract:itemPattern=^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$
	Aliases        []string    `json:"aliases"`
	Status         TenantState `json:"status"`
	ConfigRevision Revision    `json:"config_revision"`
	// TenantConfigRevision advances with tenant-local config mutations. It is
	// separate from ConfigRevision, the platform directory CAS revision.
	TenantConfigRevision Revision     `json:"tenant_config_revision,omitempty"`
	DirectoryRevision    Revision     `json:"directory_revision"`
	Deployment           DeploymentID `json:"deployment_id"`
	//contract:minLength=1
	//contract:maxLength=160
	//contract:pattern=^[A-Za-z0-9][A-Za-z0-9._:-]{1,159}$
	DataPlaneRef string `json:"data_plane_ref,omitempty"`
}

//contract:wire:v2
type SetTenantState struct {
	Meta  CommandMeta `json:"meta"`
	State TenantState `json:"state"`
	// ExpectedConfigRevision fences the transition against a stale Cloud view.
	// Retries that find the requested state already applied are idempotent.
	//contract:minLength=1
	//contract:maxLength=20
	//contract:pattern=^(0|[1-9][0-9]*)$
	ExpectedConfigRevision Revision `json:"expected_config_revision"`
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}

// ReconcileTenantStateOperation requests another execution of an uncertain
// tenant state operation. Engine accepts this only for the same durable
// set-tenant-state operation because its directory transition is revision
// fenced and idempotent; other operation kinds require their own recovery
// evidence and cannot use this request.
//
//contract:wire:v2
type ReconcileTenantStateOperation struct {
	Meta CommandMeta `json:"meta"`
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}

// ReconcileTenantProvisionOperation retries only the base tenant provisioning
// operation. Engine verifies the immutable tenant identity and assigned data
// allocation before replaying its idempotent database bootstrap; package and
// external notification effects are not included.
//
//contract:wire:v2
type ReconcileTenantProvisionOperation struct {
	Meta CommandMeta `json:"meta"`
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}

// ReconcileTenantPackageOperation requeues the same immutable package apply
// operation after the operator has reviewed its journal and tenant state.
// Engine repeats artifact digest verification and resumes schema activation
// from the durable per-tenant activation journal.
//
//contract:wire:v2
type ReconcileTenantPackageOperation struct {
	Meta CommandMeta `json:"meta"`
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}

// ReconcileTenantDeleteOperation requests another attempt of a caller-owned
// tenant deletion after Engine has verified its immutable checkpoint. The
// executor must reverify the same checkpoint and allocation before resuming
// teardown; this command never replaces that evidence.
//
//contract:wire:v2
type ReconcileTenantDeleteOperation struct {
	Meta CommandMeta `json:"meta"`
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}

//contract:wire:v2
type DeleteTenant struct {
	Meta CommandMeta `json:"meta"`
	//contract:minLength=3
	//contract:maxLength=160
	//contract:pattern=^[A-Za-z0-9][A-Za-z0-9._:-]{1,159}$
	RecoveryCheckpointRef string `json:"recovery_checkpoint_ref"`
}

// PermissionGrant is the stable JSON permission projection accepted when a
// managed channel session is issued. Engine converts this wire value into its
// internal authorization model at the HTTP boundary.
//
//contract:wire:v2
type PermissionGrant struct {
	Doctype string `json:"doctype"`
	Role    string `json:"role"`
	Read    bool   `json:"read"`
	Write   bool   `json:"write"`
	Create  bool   `json:"create"`
	Delete  bool   `json:"delete"`
	Submit  bool   `json:"submit"`
	Cancel  bool   `json:"cancel"`
	Amend   bool   `json:"amend"`
	Export  bool   `json:"export"`
	Import  bool   `json:"import"`
	Report  bool   `json:"report"`
	IfOwner bool   `json:"if_owner"`
}

var (
	managementRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{1,159}$`)
	packageDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	managementAliasLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ValidateProvisionTenant enforces bounded, opaque allocation references and
// DNS aliases. It intentionally cannot express a DSN, filesystem path, or URL.
func ValidateProvisionTenant(cmd ProvisionTenant) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if _, err := ParseTenantID(string(cmd.Tenant)); err != nil {
		return fmt.Errorf("tenant ID: %w", err)
	}
	if _, err := ParseDeploymentID(string(cmd.Deployment)); err != nil {
		return fmt.Errorf("deployment ID: %w", err)
	}
	if len(cmd.BootstrapAdminEmail) < 3 || len(cmd.BootstrapAdminEmail) > 254 {
		return fmt.Errorf("bootstrap administrator email must be between 3 and 254 bytes")
	}
	parsedEmail, err := mail.ParseAddress(cmd.BootstrapAdminEmail)
	if err != nil || parsedEmail.Address != cmd.BootstrapAdminEmail {
		return fmt.Errorf("bootstrap administrator email must be a single mailbox address")
	}
	if err := validateManagementAlias(cmd.PrimaryAlias); err != nil {
		return fmt.Errorf("primary tenant alias: %w", err)
	}
	if len(cmd.DatabaseAllocationRef) < 3 || !managementRefPattern.MatchString(cmd.DatabaseAllocationRef) || strings.Contains(cmd.DatabaseAllocationRef, "..") {
		return fmt.Errorf("database allocation reference must be an opaque provider reference")
	}
	if len(cmd.Aliases) > 49 {
		return fmt.Errorf("tenant may have at most 49 additional aliases")
	}
	seen := map[string]struct{}{cmd.PrimaryAlias: struct{}{}}
	for _, alias := range cmd.Aliases {
		if err := validateManagementAlias(alias); err != nil {
			return err
		}
		if _, exists := seen[alias]; exists {
			return fmt.Errorf("tenant aliases contain a duplicate")
		}
		seen[alias] = struct{}{}
	}
	if cmd.PackageDigest != "" && !packageDigestPattern.MatchString(cmd.PackageDigest) {
		return fmt.Errorf("package digest must be lowercase SHA-256 hex")
	}
	return nil
}

func ValidateRequestTenantInvitation(cmd RequestTenantInvitation) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if len(cmd.Email) < 3 || len(cmd.Email) > 254 {
		return fmt.Errorf("invitation email must be between 3 and 254 bytes")
	}
	parsedEmail, err := mail.ParseAddress(cmd.Email)
	if err != nil || parsedEmail.Address != cmd.Email {
		return fmt.Errorf("invitation email must be a single mailbox address")
	}
	return nil
}

func ValidateReconcileTenantInvitationOperation(cmd ReconcileTenantInvitationOperation) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if cmd.Outcome != InvitationDelivered && cmd.Outcome != InvitationNotDelivered {
		return fmt.Errorf("invitation outcome must be delivered or not_delivered")
	}
	if len(strings.TrimSpace(cmd.Reason)) < 8 || len(cmd.Reason) > 1000 {
		return fmt.Errorf("invitation reconciliation reason must be 8 to 1000 bytes")
	}
	return nil
}

func ValidateApplyPackage(cmd ApplyPackage) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if _, err := ParseTenantID(string(cmd.Tenant)); err != nil {
		return fmt.Errorf("tenant ID: %w", err)
	}
	if len(cmd.ArtifactRef) < 3 || !managementRefPattern.MatchString(cmd.ArtifactRef) || strings.Contains(cmd.ArtifactRef, "..") {
		return fmt.Errorf("artifact reference must be an opaque provider reference")
	}
	if !packageDigestPattern.MatchString(cmd.Digest) {
		return fmt.Errorf("package digest must be lowercase SHA-256 hex")
	}
	if _, err := ParseRevision(string(cmd.ExpectedConfigRevision)); err != nil {
		return fmt.Errorf("expected config revision: %w", err)
	}
	return nil
}

func ValidateSetTenantState(cmd SetTenantState) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if cmd.State != TenantActive && cmd.State != TenantSuspended {
		return fmt.Errorf("management can request only active or suspended tenant state")
	}
	if _, err := ParseRevision(string(cmd.ExpectedConfigRevision)); err != nil {
		return fmt.Errorf("expected config revision: %w", err)
	}
	if err := validateTextBytes("reason", cmd.Reason); err != nil {
		return err
	}
	return nil
}

func ValidateReconcileTenantStateOperation(cmd ReconcileTenantStateOperation) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if err := validateTextBytes("reason", cmd.Reason); err != nil {
		return err
	}
	if strings.TrimSpace(cmd.Reason) == "" {
		return fmt.Errorf("reconciliation reason is required")
	}
	return nil
}

func ValidateReconcileTenantProvisionOperation(cmd ReconcileTenantProvisionOperation) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if err := validateTextBytes("reason", cmd.Reason); err != nil {
		return err
	}
	if strings.TrimSpace(cmd.Reason) == "" {
		return fmt.Errorf("reconciliation reason is required")
	}
	return nil
}

func ValidateReconcileTenantPackageOperation(cmd ReconcileTenantPackageOperation) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if err := validateTextBytes("reason", cmd.Reason); err != nil {
		return err
	}
	if strings.TrimSpace(cmd.Reason) == "" {
		return fmt.Errorf("reconciliation reason is required")
	}
	return nil
}

func ValidateReconcileTenantDeleteOperation(cmd ReconcileTenantDeleteOperation) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if err := validateTextBytes("reason", cmd.Reason); err != nil {
		return err
	}
	if strings.TrimSpace(cmd.Reason) == "" {
		return fmt.Errorf("reconciliation reason is required")
	}
	return nil
}

func ValidateDeleteTenant(cmd DeleteTenant) error {
	if err := validateManagementMeta(cmd.Meta); err != nil {
		return err
	}
	if len(cmd.RecoveryCheckpointRef) < 3 || !managementRefPattern.MatchString(cmd.RecoveryCheckpointRef) || strings.Contains(cmd.RecoveryCheckpointRef, "..") {
		return fmt.Errorf("recovery checkpoint must be an opaque provider reference")
	}
	return nil
}

func ValidateTenantDescriptorV2(value TenantDescriptorV2) error {
	if value.Version != 2 {
		return fmt.Errorf("tenant descriptor version must be 2")
	}
	if _, err := ParseTenantID(string(value.SiteID)); err != nil {
		return fmt.Errorf("site ID: %w", err)
	}
	if value.Status != TenantProvisioning && value.Status != TenantActive && value.Status != TenantSuspended && value.Status != TenantDeleting && value.Status != TenantFailed {
		return fmt.Errorf("tenant state is invalid")
	}
	if _, err := ParseRevision(string(value.ConfigRevision)); err != nil {
		return fmt.Errorf("config revision: %w", err)
	}
	if value.TenantConfigRevision != "" {
		if _, err := ParseRevision(string(value.TenantConfigRevision)); err != nil {
			return fmt.Errorf("tenant config revision: %w", err)
		}
	}
	if _, err := ParseRevision(string(value.DirectoryRevision)); err != nil {
		return fmt.Errorf("directory revision: %w", err)
	}
	if _, err := ParseDeploymentID(string(value.Deployment)); err != nil {
		return fmt.Errorf("deployment ID: %w", err)
	}
	if len(value.Aliases) > 50 {
		return fmt.Errorf("tenant descriptor exceeds 50 aliases")
	}
	seen := make(map[string]struct{}, len(value.Aliases))
	for _, alias := range value.Aliases {
		if err := validateManagementAlias(alias); err != nil {
			return err
		}
		if _, exists := seen[alias]; exists {
			return fmt.Errorf("tenant aliases contain a duplicate")
		}
		seen[alias] = struct{}{}
	}
	if value.DataPlaneRef != "" && !managementRefPattern.MatchString(value.DataPlaneRef) {
		return fmt.Errorf("data-plane reference must be opaque")
	}
	return nil
}

// ValidateTenantAlias checks one canonical DNS alias used for management
// lookups. Callers may normalize case and a terminal dot before validation.
func ValidateTenantAlias(alias string) error {
	return validateManagementAlias(alias)
}

func ValidateManagedOperation(value ManagedOperation) error {
	if _, err := ParseOperationID(string(value.ID)); err != nil {
		return fmt.Errorf("operation ID: %w", err)
	}
	if _, err := ParseTenantID(string(value.Tenant)); err != nil {
		return fmt.Errorf("tenant ID: %w", err)
	}
	switch value.State {
	case OperationRequested, OperationRunning, OperationSucceeded, OperationFailed, OperationRecoveryRequired:
	default:
		return fmt.Errorf("management operation state is invalid")
	}
	if value.ObservedRevision != "" {
		if _, err := ParseRevision(string(value.ObservedRevision)); err != nil {
			return fmt.Errorf("observed revision: %w", err)
		}
	}
	return nil
}

func validateManagementMeta(meta CommandMeta) error {
	if _, err := ParseIdempotencyKey(string(meta.IdempotencyKey)); err != nil {
		return fmt.Errorf("idempotency key: %w", err)
	}
	return nil
}

func validateManagementAlias(alias string) error {
	if alias == "" || len(alias) > 253 || strings.HasSuffix(alias, ".") {
		return fmt.Errorf("tenant alias is not a valid DNS name")
	}
	for _, label := range strings.Split(alias, ".") {
		if !managementAliasLabel.MatchString(label) {
			return fmt.Errorf("tenant alias is not a valid DNS name")
		}
	}
	return nil
}
