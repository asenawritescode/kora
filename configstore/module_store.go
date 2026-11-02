package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func replaceModuleInstallations(existing []contract.ModuleInstallation, tenant contract.TenantID, modules []contract.ModuleDescriptor, digest string) []contract.ModuleInstallation {
	byID := make(map[contract.ModuleID]contract.ModuleInstallation, len(existing)+len(modules))
	for _, installation := range existing {
		byID[installation.ModuleID] = installation
	}
	for _, module := range modules {
		byID[module.ID] = contract.ModuleInstallation{TenantID: tenant, ModuleID: module.ID, Version: module.Version, ArtifactDigest: digest, State: "active"}
	}
	out := make([]contract.ModuleInstallation, 0, len(byID))
	for _, installation := range byID {
		out = append(out, installation)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModuleID < out[j].ModuleID })
	return out
}

func mergeInstalledDescriptors(existing, incoming []contract.ModuleDescriptor) []contract.ModuleDescriptor {
	byID := make(map[contract.ModuleID]contract.ModuleDescriptor, len(existing)+len(incoming))
	for _, m := range existing {
		byID[m.ID] = m
	}
	for _, m := range incoming {
		byID[m.ID] = m
	}
	out := make([]contract.ModuleDescriptor, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func replaceModuleOwnership(existing []contract.ResourceOwnership, tenant contract.TenantID, modules []contract.ModuleDescriptor) ([]contract.ResourceOwnership, error) {
	byResource := make(map[string]contract.ResourceOwnership, len(existing))
	moduleIDs := make(map[contract.ModuleID]bool, len(modules))
	for _, module := range modules {
		moduleIDs[module.ID] = true
	}
	for _, owner := range existing {
		if !moduleIDs[owner.OwnerModuleID] {
			byResource[owner.Resource.Namespace+"\x00"+owner.Resource.Name] = owner
		}
	}
	for _, module := range modules {
		for _, resource := range module.Resources {
			key := resource.Ref.Namespace + "\x00" + resource.Ref.Name
			if prior, exists := byResource[key]; exists && prior.OwnerModuleID != module.ID {
				return nil, fmt.Errorf("resource %q is already owned by module %q", resource.Ref.String(), prior.OwnerModuleID)
			}
			owner := contract.ResourceOwnership{TenantID: tenant, Resource: resource.Ref, OwnerModuleID: module.ID}
			byResource[key] = owner
		}
	}
	out := make([]contract.ResourceOwnership, 0, len(byResource))
	for _, owner := range byResource {
		out = append(out, owner)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Resource.Namespace != b.Resource.Namespace {
			return a.Resource.Namespace < b.Resource.Namespace
		}
		return a.Resource.Name < b.Resource.Name
	})
	return out, nil
}

// ModuleStore persists the authoritative module ownership snapshot in the
// tenant database. The activation caller commits it in the same transaction
// as the configuration snapshot.
type ModuleStore struct {
	DB      *sql.DB
	Dialect db.QueryDialect
}

func (s *ModuleStore) LoadDescriptors(tenant contract.TenantID) ([]contract.ModuleDescriptor, uint64, error) {
	snapshot, err := s.Load(tenant)
	if err != nil {
		return nil, 0, err
	}
	return append([]contract.ModuleDescriptor(nil), snapshot.Modules...), snapshot.Revision, nil
}

func NewModuleStore(database *sql.DB, dialect db.QueryDialect) *ModuleStore {
	return &ModuleStore{DB: database, Dialect: dialect}
}

func (s *ModuleStore) EnsureSchema() error {
	if s == nil || s.DB == nil || s.Dialect == nil {
		return fmt.Errorf("module store requires database and dialect")
	}
	if _, err := s.DB.Exec(s.SchemaStatement()); err != nil {
		return err
	}
	return s.EnsureCapabilityLeaseSchema(context.Background())
}

// capabilityLeaseTimeLayout keeps lease timestamps lexically sortable across
// SQL dialects that store timestamps as text. RFC3339Nano is not fixed width.
const capabilityLeaseTimeLayout = "2006-01-02T15:04:05.000000000Z"

// EnsureCapabilityLeaseSchema creates the durable effect-admission table used
// by module grants. It shares the tenant module database and SQL dialect.
func (s *ModuleStore) EnsureCapabilityLeaseSchema(ctx context.Context) error {
	if s == nil || s.DB == nil || s.Dialect == nil || ctx == nil {
		return fmt.Errorf("capability lease schema requires context, database, and dialect")
	}
	query := `CREATE TABLE IF NOT EXISTS _kora_capability_lease (tenant_id VARCHAR(140) NOT NULL, lease_id VARCHAR(160) NOT NULL, scope_json TEXT NOT NULL, caller_module_id VARCHAR(160) NOT NULL, provider_id VARCHAR(160) NOT NULL, operation_name VARCHAR(160) NOT NULL, binding_revision BIGINT NOT NULL, grant_epoch BIGINT NOT NULL, expires_at VARCHAR(40) NOT NULL, state VARCHAR(16) NOT NULL, admitted_attempts BIGINT NOT NULL, credential_ref VARCHAR(320) NOT NULL, revoked_by VARCHAR(160) NOT NULL, revocation_reason VARCHAR(1000) NOT NULL, revoked_at VARCHAR(40) NOT NULL, PRIMARY KEY (tenant_id, lease_id))`
	if driver, ok := s.Dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "mysql" {
		// The admission and revoke updates serialize only when the lease record
		// is stored in a transactional table. The module snapshot uses InnoDB
		// for the same reason.
		query += ` ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	}
	_, err := s.DB.ExecContext(ctx, query)
	return err
}

// CreateCapabilityLease persists an active grant. Lease IDs are unique within
// a tenant, and callers must issue a new epoch after revocation.
func (s *ModuleStore) CreateCapabilityLease(ctx context.Context, lease contract.CapabilityLease) error {
	if err := lease.Validate(); err != nil {
		return err
	}
	if lease.State != "active" || lease.AdmittedAttempts != 0 || !lease.ExpiresAt.After(time.Now().UTC()) {
		return fmt.Errorf("new capability lease must be active and unused")
	}
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil {
		return fmt.Errorf("capability lease creation requires context and module store")
	}
	scopeJSON, err := json.Marshal(lease.Scope)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `INSERT INTO _kora_capability_lease (tenant_id, lease_id, scope_json, caller_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, admitted_attempts, credential_ref, revoked_by, revocation_reason, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), string(lease.TenantID), lease.ID, string(scopeJSON), string(lease.CallerModuleID), lease.ProviderID, lease.Operation, lease.BindingRevision, lease.GrantEpoch, lease.ExpiresAt.UTC().Format(capabilityLeaseTimeLayout), lease.State, uint64(0), lease.CredentialRef, "", "", "")
	return err
}

// EnsureCapabilityLease makes a trusted, predeclared workflow grant durable on
// first assembly and refuses to recreate or widen an existing lease.
func (s *ModuleStore) EnsureCapabilityLease(ctx context.Context, expected contract.CapabilityLease) error {
	if ctx == nil || expected.Validate() != nil || expected.State != "active" || expected.AdmittedAttempts != 0 || !expected.ExpiresAt.After(time.Now().UTC()) {
		return fmt.Errorf("capability lease declaration is invalid")
	}
	current, found, err := s.LoadCapabilityLease(ctx, expected.TenantID, expected.ID)
	if err != nil {
		return err
	}
	if found {
		if !sameCapabilityLeaseDeclaration(current, expected) {
			return contract.NewError(contract.CodePermissionDenied, "durable capability lease differs from its declared grant")
		}
		if current.State == "revoked" || current.State == "expired" {
			// Preserve runtime availability for reconciliation. Effect admission
			// will continue to deny this durable security decision.
			return nil
		}
		current.AdmittedAttempts = 0
		if current != expected {
			return contract.NewError(contract.CodePermissionDenied, "durable capability lease epoch differs from its declared grant")
		}
		return nil
	}
	if err := s.CreateCapabilityLease(ctx, expected); err != nil {
		// Concurrent runtime assembly may insert the same grant first. Accept
		// only an exact match; revoked or widened records stay denied.
		current, found, loadErr := s.LoadCapabilityLease(ctx, expected.TenantID, expected.ID)
		if loadErr != nil {
			return err
		}
		if !found || !sameCapabilityLeaseDeclaration(current, expected) || current.State == "active" && current.GrantEpoch != expected.GrantEpoch {
			return contract.NewError(contract.CodePermissionDenied, "capability lease could not be issued or differs from its declared grant")
		}
	}
	return nil
}

func sameCapabilityLeaseDeclaration(current, expected contract.CapabilityLease) bool {
	return current.ID == expected.ID && current.TenantID == expected.TenantID && current.Scope == expected.Scope && current.CallerModuleID == expected.CallerModuleID && current.ProviderID == expected.ProviderID && current.Operation == expected.Operation && current.BindingRevision == expected.BindingRevision && current.ExpiresAt.Equal(expected.ExpiresAt) && current.CredentialRef == expected.CredentialRef
}

// LoadCapabilityLease reads the durable authority record without returning
// credential material. The found flag distinguishes a missing grant from a
// storage failure.
func (s *ModuleStore) LoadCapabilityLease(ctx context.Context, tenant contract.TenantID, leaseID string) (contract.CapabilityLease, bool, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || tenant == "" || leaseID == "" {
		return contract.CapabilityLease{}, false, fmt.Errorf("capability lease lookup identity is invalid")
	}
	lease := contract.CapabilityLease{TenantID: tenant, ID: leaseID}
	var scopeJSON, caller, expiresAt string
	var epoch, revision, attempts uint64
	if err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, `SELECT scope_json, caller_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, admitted_attempts, credential_ref FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?`), string(tenant), leaseID).Scan(&scopeJSON, &caller, &lease.ProviderID, &lease.Operation, &revision, &epoch, &expiresAt, &lease.State, &attempts, &lease.CredentialRef); err != nil {
		if err == sql.ErrNoRows {
			return contract.CapabilityLease{}, false, nil
		}
		return contract.CapabilityLease{}, false, err
	}
	if err := json.Unmarshal([]byte(scopeJSON), &lease.Scope); err != nil {
		return contract.CapabilityLease{}, false, fmt.Errorf("decode capability lease scope")
	}
	lease.CallerModuleID = contract.ModuleID(caller)
	lease.BindingRevision = revision
	lease.GrantEpoch = epoch
	lease.AdmittedAttempts = attempts
	parsedExpiry, err := time.Parse(capabilityLeaseTimeLayout, expiresAt)
	if err != nil {
		return contract.CapabilityLease{}, false, fmt.Errorf("decode capability lease expiry")
	}
	lease.ExpiresAt = parsedExpiry
	if err := lease.Validate(); err != nil {
		return contract.CapabilityLease{}, false, fmt.Errorf("validate durable capability lease")
	}
	return lease, true, nil
}

// AdmitCapabilityAttempt is the revocation linearization point. The guarded
// update serializes with RevokeCapabilityLease; once revoke commits, no later
// admission can match the active epoch.
func (s *ModuleStore) AdmitCapabilityAttempt(ctx context.Context, expected contract.CapabilityLease, now time.Time) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || expected.Validate() != nil || expected.State != "active" || now.IsZero() {
		return fmt.Errorf("capability attempt admission identity is invalid")
	}
	scopeJSON, err := json.Marshal(expected.Scope)
	if err != nil {
		return err
	}
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_capability_lease SET admitted_attempts = admitted_attempts + 1 WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at > ? AND credential_ref = ?`), string(expected.TenantID), expected.ID, string(scopeJSON), string(expected.CallerModuleID), expected.ProviderID, expected.Operation, expected.BindingRevision, expected.GrantEpoch, now.UTC().Format(capabilityLeaseTimeLayout), expected.CredentialRef)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		_, _ = s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_capability_lease SET state = 'expired' WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at <= ? AND credential_ref = ?`), string(expected.TenantID), expected.ID, string(scopeJSON), string(expected.CallerModuleID), expected.ProviderID, expected.Operation, expected.BindingRevision, expected.GrantEpoch, now.UTC().Format(capabilityLeaseTimeLayout), expected.CredentialRef)
		return contract.NewError(contract.CodePermissionDenied, "capability lease is revoked, expired, or stale")
	}
	return nil
}

// AdmitCapabilityAttemptInTx places the authorization update in the same
// transaction as a local owner command. Admission commits atomically with that
// effect, and serializes against the same revocation update.
func (s *ModuleStore) AdmitCapabilityAttemptInTx(ctx context.Context, tx *sql.Tx, expected contract.CapabilityLease, now time.Time) error {
	if ctx == nil || tx == nil || expected.Validate() != nil || expected.State != "active" || now.IsZero() {
		return fmt.Errorf("transactional capability admission identity is invalid")
	}
	scopeJSON, err := json.Marshal(expected.Scope)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_capability_lease SET admitted_attempts = admitted_attempts + 1 WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at > ? AND credential_ref = ?`), string(expected.TenantID), expected.ID, string(scopeJSON), string(expected.CallerModuleID), expected.ProviderID, expected.Operation, expected.BindingRevision, expected.GrantEpoch, now.UTC().Format(capabilityLeaseTimeLayout), expected.CredentialRef)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		_, _ = tx.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_capability_lease SET state = 'expired' WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at <= ? AND credential_ref = ?`), string(expected.TenantID), expected.ID, string(scopeJSON), string(expected.CallerModuleID), expected.ProviderID, expected.Operation, expected.BindingRevision, expected.GrantEpoch, now.UTC().Format(capabilityLeaseTimeLayout), expected.CredentialRef)
		return contract.NewError(contract.CodePermissionDenied, "capability lease is revoked, expired, or stale")
	}
	return nil
}

// ValidateCapabilityLease checks that the complete durable grant still matches
// the grant pinned by the caller after a runtime restart or reload.
func (s *ModuleStore) ValidateCapabilityLease(ctx context.Context, expected contract.CapabilityLease, now time.Time) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || expected.Validate() != nil || now.IsZero() {
		return fmt.Errorf("capability lease validation identity is invalid")
	}
	var scopeJSON, caller, provider, operation, expiresAt, state, credentialRef string
	var bindingRevision, grantEpoch uint64
	err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, `SELECT scope_json, caller_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, credential_ref FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?`), string(expected.TenantID), expected.ID).Scan(&scopeJSON, &caller, &provider, &operation, &bindingRevision, &grantEpoch, &expiresAt, &state, &credentialRef)
	if err != nil {
		return fmt.Errorf("capability lease is unavailable")
	}
	wantScope, err := json.Marshal(expected.Scope)
	if err != nil {
		return err
	}
	wantExpiry := expected.ExpiresAt.UTC().Format(capabilityLeaseTimeLayout)
	if scopeJSON != string(wantScope) || caller != string(expected.CallerModuleID) || provider != expected.ProviderID || operation != expected.Operation || bindingRevision != expected.BindingRevision || grantEpoch != expected.GrantEpoch || expiresAt != wantExpiry || credentialRef != expected.CredentialRef || state != "active" || expiresAt <= now.UTC().Format(capabilityLeaseTimeLayout) {
		return fmt.Errorf("capability lease is revoked, expired, or stale")
	}
	return nil
}

// RevokeCapabilities applies one scope and binding-revision fenced security
// decision to every active lease for the exact selector. The single UPDATE is
// the revocation linearization point shared with attempt admission.
func (s *ModuleStore) RevokeCapabilities(ctx context.Context, request contract.RevokeCapability, actor string, now time.Time) (int64, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || request.Validate() != nil || strings.TrimSpace(actor) == "" || len(actor) > 160 || now.IsZero() {
		return 0, fmt.Errorf("capability revocation request is invalid")
	}
	scopeJSON, err := json.Marshal(request.Scope)
	if err != nil {
		return 0, err
	}
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_capability_lease SET grant_epoch = grant_epoch + 1, state = 'revoked', revoked_by = ?, revocation_reason = ?, revoked_at = ? WHERE tenant_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND state = 'active'`), actor, request.Reason, now.UTC().Format(capabilityLeaseTimeLayout), string(request.Scope.TenantID), string(scopeJSON), string(request.Selector.CallerModuleID), request.Selector.ProviderID, request.Selector.Operation, request.ExpectedRevision)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, contract.NewError(contract.CodeConflict, "capability revocation revision is stale or no active lease matches")
	}
	return count, nil
}

// RevokeCapabilityLease advances the epoch and records the security decision
// atomically. The expected epoch prevents stale revokers from changing a newer grant.
func (s *ModuleStore) RevokeCapabilityLease(ctx context.Context, expected contract.CapabilityLease, actor, reason string, now time.Time) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || expected.Validate() != nil || expected.State != "active" || actor == "" || strings.TrimSpace(reason) == "" || len(reason) > 1000 || now.IsZero() {
		return fmt.Errorf("capability revocation requires tenant, lease, expected epoch, actor, reason, and time")
	}
	scopeJSON, err := json.Marshal(expected.Scope)
	if err != nil {
		return err
	}
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_capability_lease SET grant_epoch = grant_epoch + 1, state = 'revoked', revoked_by = ?, revocation_reason = ?, revoked_at = ? WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active'`), actor, reason, now.UTC().Format(capabilityLeaseTimeLayout), string(expected.TenantID), expected.ID, string(scopeJSON), string(expected.CallerModuleID), expected.ProviderID, expected.Operation, expected.BindingRevision, expected.GrantEpoch)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("capability lease revoke revision conflict or already revoked")
	}
	return nil
}

func (s *ModuleStore) SchemaStatement() string {
	if driver, ok := s.Dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "mysql" {
		return `CREATE TABLE IF NOT EXISTS _kora_module_snapshot (tenant_id VARCHAR(140) PRIMARY KEY, revision BIGINT NOT NULL, snapshot_json LONGTEXT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	}
	return `CREATE TABLE IF NOT EXISTS _kora_module_snapshot (tenant_id TEXT PRIMARY KEY, revision BIGINT NOT NULL, snapshot_json TEXT NOT NULL)`
}

func (s *ModuleStore) EnsureTxSchema(tx *sql.Tx) error {
	if tx == nil || s == nil || s.Dialect == nil {
		return fmt.Errorf("module store transaction and dialect are required")
	}
	_, err := tx.Exec(s.SchemaStatement())
	return err
}

func (s *ModuleStore) Load(tenant contract.TenantID) (contract.ModuleSnapshot, error) {
	if s == nil || s.DB == nil || s.Dialect == nil || tenant == "" {
		return contract.ModuleSnapshot{}, fmt.Errorf("module snapshot tenant is required")
	}
	var revision uint64
	var payload string
	err := s.DB.QueryRow(db.Rebind(s.Dialect, `SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`), string(tenant)).Scan(&revision, &payload)
	if err == sql.ErrNoRows {
		return contract.ModuleSnapshot{TenantID: tenant}, nil
	}
	if err != nil {
		return contract.ModuleSnapshot{}, err
	}
	var snapshot contract.ModuleSnapshot
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return contract.ModuleSnapshot{}, fmt.Errorf("decode module snapshot: %w", err)
	}
	if snapshot.TenantID != tenant || snapshot.Revision != revision {
		return contract.ModuleSnapshot{}, fmt.Errorf("module snapshot identity or revision mismatch")
	}
	if err := validateModuleSnapshot(snapshot); err != nil {
		return contract.ModuleSnapshot{}, fmt.Errorf("validate module snapshot: %w", err)
	}
	return snapshot, nil
}

func (s *ModuleStore) CommitActivation(ctx context.Context, tx *sql.Tx, expectedRevision uint64, candidate contract.ModuleSnapshot) (uint64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("module activation requires context")
	}
	if tx == nil || candidate.TenantID == "" {
		return 0, fmt.Errorf("module activation requires transaction and tenant")
	}
	if err := validateModuleSnapshot(candidate); err != nil {
		return 0, err
	}
	current, err := loadModuleSnapshotTx(ctx, tx, s.Dialect, candidate.TenantID)
	if err != nil {
		return 0, err
	}
	if current.Revision != expectedRevision {
		return 0, fmt.Errorf("module snapshot revision conflict: expected %d, actual %d", expectedRevision, current.Revision)
	}
	candidate.Revision = expectedRevision + 1
	payload, err := json.Marshal(candidate)
	if err != nil {
		return 0, err
	}
	if expectedRevision == 0 {
		_, err = tx.ExecContext(ctx, db.Rebind(s.Dialect, `INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)`), string(candidate.TenantID), candidate.Revision, string(payload))
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_module_snapshot SET revision = ?, snapshot_json = ? WHERE tenant_id = ? AND revision = ?`), candidate.Revision, string(payload), string(candidate.TenantID), expectedRevision)
		if err == nil {
			var affected int64
			affected, err = result.RowsAffected()
			if err == nil && affected != 1 {
				err = fmt.Errorf("module snapshot revision conflict")
			}
		}
	}
	if err != nil {
		return 0, fmt.Errorf("commit module snapshot: %w", err)
	}
	return candidate.Revision, nil
}

func (s *ModuleStore) CommitActivationTx(ctx context.Context, tx *sql.Tx, expectedRevision uint64, candidate contract.ModuleSnapshot) (uint64, error) {
	return s.CommitActivation(ctx, tx, expectedRevision, candidate)
}

// CommitProviderSelections persists explicit provider choices under the same
// tenant snapshot compare-and-increment fence used for module activation.
func (s *ModuleStore) CommitProviderSelections(ctx context.Context, tenant contract.TenantID, expectedRevision uint64, selections []contract.ProviderSelection) (contract.ModuleSnapshot, error) {
	if s == nil || s.DB == nil || s.Dialect == nil {
		return contract.ModuleSnapshot{}, fmt.Errorf("module store requires database and dialect")
	}
	if ctx == nil {
		return contract.ModuleSnapshot{}, fmt.Errorf("provider binding update requires context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.ModuleSnapshot{}, fmt.Errorf("provider binding update requires a valid tenant")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return contract.ModuleSnapshot{}, fmt.Errorf("begin provider binding update: %w", err)
	}
	defer tx.Rollback()
	candidate, err := loadModuleSnapshotTx(ctx, tx, s.Dialect, tenant)
	if err != nil {
		return contract.ModuleSnapshot{}, err
	}
	if candidate.Revision != expectedRevision {
		return contract.ModuleSnapshot{}, fmt.Errorf("module snapshot revision conflict: expected %d, actual %d", expectedRevision, candidate.Revision)
	}
	candidate.ProviderSelections = append([]contract.ProviderSelection(nil), selections...)
	newRevision, err := s.CommitActivation(ctx, tx, expectedRevision, candidate)
	if err != nil {
		return contract.ModuleSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return contract.ModuleSnapshot{}, fmt.Errorf("commit provider binding update: %w", err)
	}
	candidate.Revision = newRevision
	return candidate, nil
}

func loadModuleSnapshotTx(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect, tenant contract.TenantID) (contract.ModuleSnapshot, error) {
	if ctx == nil {
		return contract.ModuleSnapshot{}, fmt.Errorf("module snapshot read requires context")
	}
	var revision uint64
	var payload string
	err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`), string(tenant)).Scan(&revision, &payload)
	if err == sql.ErrNoRows {
		return contract.ModuleSnapshot{TenantID: tenant}, nil
	}
	if err != nil {
		return contract.ModuleSnapshot{}, err
	}
	var snapshot contract.ModuleSnapshot
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return contract.ModuleSnapshot{}, err
	}
	snapshot.Revision = revision
	return snapshot, nil
}

func validateModuleSnapshot(snapshot contract.ModuleSnapshot) error {
	if err := contract.ValidateInstalledPackages(snapshot.Packages); err != nil {
		return err
	}
	modules := make(map[contract.ModuleID]bool, len(snapshot.Installations))
	for _, installation := range snapshot.Installations {
		if installation.TenantID != snapshot.TenantID || installation.ModuleID == "" || installation.Version == "" || installation.ArtifactDigest == "" {
			return fmt.Errorf("invalid module installation")
		}
		if modules[installation.ModuleID] {
			return fmt.Errorf("duplicate installed module %q", installation.ModuleID)
		}
		modules[installation.ModuleID] = true
	}
	if err := contract.ValidateModuleSet(snapshot.Modules, nil); err != nil {
		return err
	}
	installed := make(map[contract.ModuleID]bool, len(snapshot.Installations))
	for _, installation := range snapshot.Installations {
		installed[installation.ModuleID] = true
	}
	expectedOwnership := make(map[string]contract.ModuleID)
	for _, module := range snapshot.Modules {
		installation, ok := func() (contract.ModuleInstallation, bool) {
			for _, value := range snapshot.Installations {
				if value.ModuleID == module.ID {
					return value, true
				}
			}
			return contract.ModuleInstallation{}, false
		}()
		if !installed[module.ID] || !ok || installation.Version != module.Version {
			return fmt.Errorf("module descriptor %q has no installation", module.ID)
		}
		for _, resource := range module.Resources {
			key := resource.Ref.Namespace + "/" + resource.Ref.Name
			if previous, exists := expectedOwnership[key]; exists && previous != module.ID {
				return fmt.Errorf("resource %q has duplicate module owners %q and %q", key, previous, module.ID)
			}
			expectedOwnership[key] = module.ID
		}
	}
	owners := map[string]contract.ModuleID{}
	for _, owner := range snapshot.Ownership {
		if owner.TenantID != snapshot.TenantID || owner.OwnerModuleID == "" || owner.Resource.Name == "" || owner.Resource.Namespace == "" {
			return fmt.Errorf("invalid resource ownership")
		}
		if !modules[owner.OwnerModuleID] {
			return fmt.Errorf("resource owner %q is not installed", owner.OwnerModuleID)
		}
		key := owner.Resource.Namespace + "/" + owner.Resource.Name
		if previous, exists := owners[key]; exists {
			return fmt.Errorf("resource %q has duplicate owners %q and %q", key, previous, owner.OwnerModuleID)
		}
		owners[key] = owner.OwnerModuleID
	}
	if len(owners) != len(expectedOwnership) {
		return fmt.Errorf("module ownership index does not match module descriptors")
	}
	for resource, expectedOwner := range expectedOwnership {
		if actualOwner, ok := owners[resource]; !ok || actualOwner != expectedOwner {
			return fmt.Errorf("resource %q ownership index does not match module descriptor", resource)
		}
	}
	moduleByID := make(map[contract.ModuleID]contract.ModuleDescriptor, len(snapshot.Modules))
	for _, module := range snapshot.Modules {
		moduleByID[module.ID] = module
	}
	seenSelections := make(map[contract.BindingKey]bool, len(snapshot.ProviderSelections))
	for _, selection := range snapshot.ProviderSelections {
		key := selection.Key
		module, installed := moduleByID[key.ConsumerModuleID]
		if !installed || key.Scope.TenantID != snapshot.TenantID || selection.Validate() != nil {
			return fmt.Errorf("invalid persisted provider selection")
		}
		if seenSelections[key] {
			return fmt.Errorf("duplicate persisted provider selection for module %q capability %q", key.ConsumerModuleID, key.CapabilityName)
		}
		seenSelections[key] = true
		declared := false
		for _, requirement := range module.Requires {
			declared = declared || requirement.Name == key.CapabilityName
		}
		for _, query := range module.Queries {
			declared = declared || query.Capability == key.CapabilityName
		}
		if !declared {
			return fmt.Errorf("persisted provider selection does not match a declared module capability")
		}
	}
	return nil
}

var _ contract.ModuleStore = (*ModuleStore)(nil)
