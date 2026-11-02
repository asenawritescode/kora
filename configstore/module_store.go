package configstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

// PlanModuleLifecycleChange computes a stable, bounded plan against one
// persisted module snapshot. Open-work inspection is deliberately supplied by
// the domain owners instead of teaching the registry about domain tables.
func PlanModuleLifecycleChange(ctx context.Context, snapshot contract.ModuleSnapshot, request contract.PlanModuleChange, inspectors ...contract.OpenWorkInspector) (contract.ModuleChangePlan, error) {
	if ctx == nil || snapshot.TenantID == "" || request.ModuleID == "" || request.ExpectedRevision != snapshot.Revision {
		return contract.ModuleChangePlan{}, fmt.Errorf("module change request is stale or incomplete")
	}
	var installation *contract.ModuleInstallation
	for i := range snapshot.Installations {
		if snapshot.Installations[i].ModuleID == request.ModuleID {
			installation = &snapshot.Installations[i]
			break
		}
	}
	if installation == nil {
		return contract.ModuleChangePlan{}, fmt.Errorf("module installation %q was not found", request.ModuleID)
	}
	if contract.ModuleLifecycleState(installation.State) == request.TargetState && len(request.ProviderSelections) == 0 {
		return contract.ModuleChangePlan{}, fmt.Errorf("module change has no state or provider binding change")
	}
	if contract.ModuleLifecycleState(installation.State) != request.TargetState {
		if err := contract.ValidateModuleLifecycleTransition(contract.ModuleLifecycleState(installation.State), request.TargetState); err != nil {
			return contract.ModuleChangePlan{}, err
		}
	}
	plan := contract.ModuleChangePlan{ModuleID: request.ModuleID, TargetState: request.TargetState, TargetVersion: request.TargetVersion, ProviderSelections: append([]contract.ProviderSelection(nil), request.ProviderSelections...), BaseRevision: snapshot.Revision}
	var targetModule *contract.ModuleDescriptor
	for i := range snapshot.Modules {
		if snapshot.Modules[i].ID == request.ModuleID {
			targetModule = &snapshot.Modules[i]
			break
		}
	}
	if request.TargetVersion != "" && request.TargetVersion != installation.Version {
		plan.DependencyBlockers = append(plan.DependencyBlockers, contract.ModuleBlocker{Code: "version_requires_verified_package_activation", Count: 1})
	}
	selectionMap := make(map[contract.BindingKey]string, len(snapshot.ProviderSelections)+len(request.ProviderSelections))
	for _, selection := range snapshot.ProviderSelections {
		selectionMap[selection.Key] = selection.ProviderID
	}
	seenSelectionChanges := map[contract.BindingKey]bool{}
	for _, selection := range request.ProviderSelections {
		if selection.Validate() != nil || selection.Key.Scope.TenantID != snapshot.TenantID || seenSelectionChanges[selection.Key] {
			return contract.ModuleChangePlan{}, fmt.Errorf("provider cutover selection is invalid or duplicated")
		}
		declared := false
		if targetModule != nil {
			for _, requirement := range targetModule.Requires {
				if requirement.Name == selection.Key.CapabilityName {
					declared = true
				}
			}
			for _, query := range targetModule.Queries {
				if query.Capability == selection.Key.CapabilityName {
					declared = true
				}
			}
		}
		if !declared {
			return contract.ModuleChangePlan{}, fmt.Errorf("provider cutover is not declared by target module")
		}
		seenSelectionChanges[selection.Key] = true
		previous := selectionMap[selection.Key]
		if previous == selection.ProviderID {
			continue
		}
		plan.BindingChanges = append(plan.BindingChanges, string(selection.Key.ConsumerModuleID)+"/"+string(selection.Key.CapabilityName)+":"+previous+"->"+selection.ProviderID)
		selectionMap[selection.Key] = selection.ProviderID
	}
	sort.Slice(plan.ProviderSelections, func(i, j int) bool {
		a, b := plan.ProviderSelections[i].Key, plan.ProviderSelections[j].Key
		if a.Scope.TenantID != b.Scope.TenantID {
			return a.Scope.TenantID < b.Scope.TenantID
		}
		if a.Scope.LegalEntityID != b.Scope.LegalEntityID {
			return a.Scope.LegalEntityID < b.Scope.LegalEntityID
		}
		if a.ConsumerModuleID != b.ConsumerModuleID {
			return a.ConsumerModuleID < b.ConsumerModuleID
		}
		if a.CapabilityName != b.CapabilityName {
			return a.CapabilityName < b.CapabilityName
		}
		return plan.ProviderSelections[i].ProviderID < plan.ProviderSelections[j].ProviderID
	})
	sort.Strings(plan.BindingChanges)
	if contract.ModuleLifecycleState(installation.State) == request.TargetState && len(plan.BindingChanges) == 0 {
		return contract.ModuleChangePlan{}, fmt.Errorf("module change contains no effective state or provider binding change")
	}
	for _, module := range snapshot.Modules {
		if request.TargetState != contract.ModuleDisabled && request.TargetState != contract.ModuleRemoved {
			break
		}
		if module.ID == request.ModuleID {
			continue
		}
		for _, requirement := range module.Requires {
			if !requirement.Required {
				continue
			}
			for _, provider := range snapshot.Modules {
				if provider.ID != request.ModuleID {
					continue
				}
				for _, offer := range provider.Provides {
					if offer.Name == requirement.Name && offer.Contract == requirement.Contract {
						plan.DependencyBlockers = append(plan.DependencyBlockers, contract.ModuleBlocker{Code: "required_by:" + string(module.ID), Count: 1})
					}
				}
			}
		}
	}
	if request.TargetState == contract.ModuleDisabled || request.TargetState == contract.ModuleRemoved {
		registeredDomains := map[string]bool{}
		for _, inspector := range inspectors {
			if inspector == nil {
				continue
			}
			if coverage, ok := inspector.(contract.OpenWorkCoverageInspector); ok {
				for _, domain := range coverage.OpenWorkDomains() {
					if domain != "" {
						registeredDomains[domain] = true
					}
				}
				continue
			}
			if domainInspector, ok := inspector.(contract.OpenWorkDomainInspector); ok && domainInspector.OpenWorkDomain() != "" {
				registeredDomains[domainInspector.OpenWorkDomain()] = true
			}
		}
		for _, domain := range []string{"domain_obligation", "event_delivery", "external_outcome", "scheduled_execution", "workflow_instance"} {
			if !registeredDomains[domain] {
				plan.OpenWorkBlockers = append(plan.OpenWorkBlockers, contract.ModuleBlocker{Code: "open_work_inspector_unavailable:" + domain, Count: 1})
			}
		}
		for _, inspector := range inspectors {
			if inspector == nil {
				continue
			}
			blockers, err := inspector.InspectOpenWork(ctx, snapshot.TenantID, request.ModuleID, 20)
			if err != nil {
				return contract.ModuleChangePlan{}, fmt.Errorf("inspect module open work: %w", err)
			}
			if len(blockers) > 100 {
				return contract.ModuleChangePlan{}, fmt.Errorf("module open-work inspector exceeded blocker bound")
			}
			for _, blocker := range blockers {
				if !validModuleBlocker(blocker) {
					return contract.ModuleChangePlan{}, fmt.Errorf("module open-work inspector returned an invalid blocker")
				}
			}
			plan.OpenWorkBlockers = append(plan.OpenWorkBlockers, blockers...)
		}
	}
	plan.DependencyBlockers = normalizeModuleBlockers(plan.DependencyBlockers)
	plan.OpenWorkBlockers = normalizeModuleBlockers(plan.OpenWorkBlockers)
	encoded, err := json.Marshal(struct {
		Tenant       contract.TenantID             `json:"tenant"`
		Module       contract.ModuleID             `json:"module"`
		Target       contract.ModuleLifecycleState `json:"target"`
		Version      string                        `json:"version"`
		Selections   []contract.ProviderSelection  `json:"provider_selections"`
		Revision     uint64                        `json:"revision"`
		Dependencies []contract.ModuleBlocker      `json:"dependencies"`
		OpenWork     []contract.ModuleBlocker      `json:"open_work"`
	}{snapshot.TenantID, request.ModuleID, request.TargetState, request.TargetVersion, plan.ProviderSelections, snapshot.Revision, plan.DependencyBlockers, plan.OpenWorkBlockers})
	if err != nil {
		return contract.ModuleChangePlan{}, err
	}
	digest := sha256.Sum256(encoded)
	plan.PlanHash = hex.EncodeToString(digest[:])
	return plan, nil
}

func validModuleBlocker(blocker contract.ModuleBlocker) bool {
	if blocker.Code == "" || len(blocker.Code) > 160 || blocker.Count == 0 || len(blocker.Examples) > 5 {
		return false
	}
	for _, example := range blocker.Examples {
		if example == "" || len(example) > 160 {
			return false
		}
	}
	return true
}

func normalizeModuleBlockers(blockers []contract.ModuleBlocker) []contract.ModuleBlocker {
	byCode := make(map[string]contract.ModuleBlocker, len(blockers))
	for _, blocker := range blockers {
		current := byCode[blocker.Code]
		if ^uint32(0)-current.Count < blocker.Count {
			current.Count = ^uint32(0)
		} else {
			current.Count += blocker.Count
		}
		for _, example := range blocker.Examples {
			if len(current.Examples) >= 5 {
				break
			}
			found := false
			for _, prior := range current.Examples {
				if prior == example {
					found = true
					break
				}
			}
			if !found {
				current.Examples = append(current.Examples, example)
			}
		}
		current.Code = blocker.Code
		byCode[blocker.Code] = current
	}
	out := make([]contract.ModuleBlocker, 0, len(byCode))
	for _, blocker := range byCode {
		sort.Strings(blocker.Examples)
		out = append(out, blocker)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// ApplyModuleLifecycleChangeTx applies the exact target captured by a fresh
// plan in the caller's transaction and increments both lifecycle and snapshot
// revisions atomically. The caller must recompute blockers in that transaction
// immediately before calling this method.
func (s *ModuleStore) ApplyModuleLifecycleChangeTx(ctx context.Context, tx *sql.Tx, tenant contract.TenantID, operation contract.OperationID, expectedRevision uint64, plan contract.ModuleChangePlan, inspectors ...contract.OpenWorkInspector) (uint64, error) {
	if s == nil || s.Dialect == nil || ctx == nil || tx == nil || tenant == "" || operation == "" || plan.ModuleID == "" || plan.PlanHash == "" || plan.BaseRevision != expectedRevision || len(plan.DependencyBlockers) != 0 || len(plan.OpenWorkBlockers) != 0 {
		return 0, fmt.Errorf("module lifecycle apply requires an unblocked current plan and transaction")
	}
	// Acquire the same tenant lifecycle row fence used by every module-owned
	// work admission before inspecting work. This prevents a new workflow or
	// command from committing between the final open-work scan and disable.
	if _, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`), string(tenant)); err != nil {
		return 0, fmt.Errorf("fence module lifecycle apply against new work: %w", err)
	}
	snapshot, err := loadModuleSnapshotTx(ctx, tx, s.Dialect, tenant)
	if err != nil {
		return 0, err
	}
	if snapshot.Revision != expectedRevision {
		return 0, fmt.Errorf("module snapshot revision conflict: expected %d, actual %d", expectedRevision, snapshot.Revision)
	}
	currentPlan, err := PlanModuleLifecycleChange(ctx, snapshot, contract.PlanModuleChange{ModuleID: plan.ModuleID, TargetState: plan.TargetState, TargetVersion: plan.TargetVersion, ProviderSelections: plan.ProviderSelections, ExpectedRevision: expectedRevision}, inspectors...)
	if err != nil {
		return 0, err
	}
	if currentPlan.PlanHash != plan.PlanHash {
		return 0, fmt.Errorf("module lifecycle plan changed or is stale")
	}
	if err := contract.ValidateModuleChangePlan(currentPlan, expectedRevision, currentPlan.OpenWorkBlockers); err != nil {
		return 0, err
	}
	var found bool
	selectionMap := make(map[contract.BindingKey]contract.ProviderSelection, len(snapshot.ProviderSelections)+len(plan.ProviderSelections))
	for _, selection := range snapshot.ProviderSelections {
		selectionMap[selection.Key] = selection
	}
	for _, selection := range plan.ProviderSelections {
		selectionMap[selection.Key] = selection
	}
	snapshot.ProviderSelections = snapshot.ProviderSelections[:0]
	for _, selection := range selectionMap {
		snapshot.ProviderSelections = append(snapshot.ProviderSelections, selection)
	}
	sort.Slice(snapshot.ProviderSelections, func(i, j int) bool {
		a, b := snapshot.ProviderSelections[i].Key, snapshot.ProviderSelections[j].Key
		if a.Scope.TenantID != b.Scope.TenantID {
			return a.Scope.TenantID < b.Scope.TenantID
		}
		if a.Scope.LegalEntityID != b.Scope.LegalEntityID {
			return a.Scope.LegalEntityID < b.Scope.LegalEntityID
		}
		if a.ConsumerModuleID != b.ConsumerModuleID {
			return a.ConsumerModuleID < b.ConsumerModuleID
		}
		return a.CapabilityName < b.CapabilityName
	})
	for i := range snapshot.Installations {
		installation := &snapshot.Installations[i]
		if installation.ModuleID != plan.ModuleID {
			continue
		}
		if contract.ModuleLifecycleState(installation.State) != plan.TargetState {
			if err := contract.ValidateModuleLifecycleTransition(contract.ModuleLifecycleState(installation.State), plan.TargetState); err != nil {
				return 0, err
			}
		}
		installation.State = string(plan.TargetState)
		installation.Revision++
		installation.StateChangedAt = time.Now().UTC()
		installation.LastLifecycleOperationID = operation
		found = true
		break
	}
	if !found {
		return 0, fmt.Errorf("module installation %q was not found", plan.ModuleID)
	}
	return s.CommitActivation(ctx, tx, expectedRevision, snapshot)
}

func replaceModuleInstallations(existing []contract.ModuleInstallation, tenant contract.TenantID, modules []contract.ModuleDescriptor, digest string) []contract.ModuleInstallation {
	byID := make(map[contract.ModuleID]contract.ModuleInstallation, len(existing)+len(modules))
	for _, installation := range existing {
		byID[installation.ModuleID] = installation
	}
	for _, module := range modules {
		installation, exists := byID[module.ID]
		if !exists {
			installation = contract.ModuleInstallation{TenantID: tenant, ModuleID: module.ID, State: string(contract.ModuleActive)}
		} else if installation.State == "" {
			// Older snapshots predate lifecycle states. Treat their installed
			// modules as active without changing their lifecycle revision.
			installation.State = string(contract.ModuleActive)
		}
		installation.TenantID = tenant
		installation.ModuleID = module.ID
		installation.Version = module.Version
		installation.ArtifactDigest = digest
		installation.Revision++
		byID[module.ID] = installation
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
	query := `CREATE TABLE IF NOT EXISTS _kora_capability_lease (tenant_id VARCHAR(140) NOT NULL, lease_id VARCHAR(160) NOT NULL, scope_json TEXT NOT NULL, caller_module_id VARCHAR(160) NOT NULL, provider_module_id VARCHAR(160) NOT NULL, provider_id VARCHAR(160) NOT NULL, operation_name VARCHAR(160) NOT NULL, binding_revision BIGINT NOT NULL, grant_epoch BIGINT NOT NULL, expires_at VARCHAR(40) NOT NULL, state VARCHAR(16) NOT NULL, admitted_attempts BIGINT NOT NULL, credential_ref VARCHAR(320) NOT NULL, revoked_by VARCHAR(160) NOT NULL, revocation_reason VARCHAR(1000) NOT NULL, revoked_at VARCHAR(40) NOT NULL, PRIMARY KEY (tenant_id, lease_id))`
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
	_, err = s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `INSERT INTO _kora_capability_lease (tenant_id, lease_id, scope_json, caller_module_id, provider_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, admitted_attempts, credential_ref, revoked_by, revocation_reason, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), string(lease.TenantID), lease.ID, string(scopeJSON), string(lease.CallerModuleID), string(lease.ProviderModuleID), lease.ProviderID, lease.Operation, lease.BindingRevision, lease.GrantEpoch, lease.ExpiresAt.UTC().Format(capabilityLeaseTimeLayout), lease.State, uint64(0), lease.CredentialRef, "", "", "")
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
	return current.ID == expected.ID && current.TenantID == expected.TenantID && current.Scope == expected.Scope && current.CallerModuleID == expected.CallerModuleID && current.ProviderModuleID == expected.ProviderModuleID && current.ProviderID == expected.ProviderID && current.Operation == expected.Operation && current.BindingRevision == expected.BindingRevision && current.ExpiresAt.Equal(expected.ExpiresAt) && current.CredentialRef == expected.CredentialRef
}

// LoadCapabilityLease reads the durable authority record without returning
// credential material. The found flag distinguishes a missing grant from a
// storage failure.
func (s *ModuleStore) LoadCapabilityLease(ctx context.Context, tenant contract.TenantID, leaseID string) (contract.CapabilityLease, bool, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || tenant == "" || leaseID == "" {
		return contract.CapabilityLease{}, false, fmt.Errorf("capability lease lookup identity is invalid")
	}
	lease := contract.CapabilityLease{TenantID: tenant, ID: leaseID}
	var scopeJSON, caller, providerModule, expiresAt string
	var epoch, revision, attempts uint64
	if err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, `SELECT scope_json, caller_module_id, provider_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, admitted_attempts, credential_ref FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?`), string(tenant), leaseID).Scan(&scopeJSON, &caller, &providerModule, &lease.ProviderID, &lease.Operation, &revision, &epoch, &expiresAt, &lease.State, &attempts, &lease.CredentialRef); err != nil {
		if err == sql.ErrNoRows {
			return contract.CapabilityLease{}, false, nil
		}
		return contract.CapabilityLease{}, false, err
	}
	if err := json.Unmarshal([]byte(scopeJSON), &lease.Scope); err != nil {
		return contract.CapabilityLease{}, false, fmt.Errorf("decode capability lease scope")
	}
	lease.CallerModuleID = contract.ModuleID(caller)
	lease.ProviderModuleID = contract.ModuleID(providerModule)
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
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`), string(expected.TenantID)); err != nil {
		return fmt.Errorf("fence capability admission against module lifecycle: %w", err)
	}
	snapshot, err := loadModuleSnapshotTx(ctx, tx, s.Dialect, expected.TenantID)
	if err != nil {
		return err
	}
	for _, moduleID := range []contract.ModuleID{expected.CallerModuleID, expected.ProviderModuleID} {
		if !moduleLifecycleActive(snapshot, moduleID) {
			return contract.NewError(contract.CodePermissionDenied, "capability caller module is not active")
		}
	}
	if err := s.admitCapabilityLeaseTx(ctx, tx, expected, now); err != nil {
		return err
	}
	return tx.Commit()
}

// AdmitPinnedCapabilityRecovery admits a pre-drain workflow attempt against
// its stored provider lease. It permits a draining module only when the
// original workflow attempt timestamp predates its drain transition.
func (s *ModuleStore) AdmitPinnedCapabilityRecovery(ctx context.Context, dispatch contract.PinnedDispatch, expected contract.CapabilityLease, tenant contract.TenantID, instance contract.InstanceID, step contract.StepID, now time.Time) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || dispatch.Validate() != nil || expected.Validate() != nil || expected.State != "active" || tenant != expected.TenantID || instance == "" || step == "" || now.IsZero() {
		return fmt.Errorf("pinned capability recovery admission identity is invalid")
	}
	var attemptStartedAt time.Time
	var raw string
	if err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, `SELECT pinned_dispatch_json,started_at FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND step_id=? AND execution_id=? AND pinned_dispatch_json IS NOT NULL ORDER BY attempt LIMIT 1`), string(tenant), string(instance), string(step), dispatch.ExecutionID).Scan(&raw, &attemptStartedAt); err != nil || attemptStartedAt.IsZero() {
		return contract.NewError(contract.CodePermissionDenied, "pinned workflow attempt start is unavailable")
	}
	var stored struct {
		Dispatch contract.PinnedDispatch
		Lease    contract.CapabilityLease
	}
	if json.Unmarshal([]byte(raw), &stored) != nil || stored.Dispatch.Validate() != nil || stored.Lease.Validate() != nil || stored.Dispatch.ProviderModuleID != dispatch.ProviderModuleID || stored.Dispatch.ProviderID != dispatch.ProviderID || stored.Dispatch.BindingRevision != dispatch.BindingRevision || stored.Dispatch.IdempotencyKey != dispatch.IdempotencyKey || stored.Dispatch.InputHash != dispatch.InputHash || stored.Lease.ID != expected.ID || stored.Lease.GrantEpoch != expected.GrantEpoch {
		return contract.NewError(contract.CodePermissionDenied, "pinned workflow recovery identity does not match its stored attempt")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`), string(expected.TenantID)); err != nil {
		return err
	}
	snapshot, err := loadModuleSnapshotTx(ctx, tx, s.Dialect, expected.TenantID)
	if err != nil {
		return err
	}
	for _, moduleID := range []contract.ModuleID{expected.CallerModuleID, expected.ProviderModuleID} {
		state, changedAt := moduleLifecycle(snapshot, moduleID)
		if state != contract.ModuleActive && !(state == contract.ModuleDraining && !changedAt.IsZero() && !attemptStartedAt.After(changedAt)) {
			return contract.NewError(contract.CodePermissionDenied, "pinned capability recovery is not authorized by module lifecycle state")
		}
	}
	if err := s.admitCapabilityLeaseTx(ctx, tx, expected, now); err != nil {
		return err
	}
	return tx.Commit()
}

func moduleLifecycle(snapshot contract.ModuleSnapshot, moduleID contract.ModuleID) (contract.ModuleLifecycleState, time.Time) {
	for _, installation := range snapshot.Installations {
		if installation.ModuleID == moduleID {
			return contract.ModuleLifecycleState(installation.State), installation.StateChangedAt
		}
	}
	return "", time.Time{}
}

func moduleLifecycleActive(snapshot contract.ModuleSnapshot, moduleID contract.ModuleID) bool {
	for _, installation := range snapshot.Installations {
		if installation.ModuleID == moduleID {
			return contract.ModuleLifecycleState(installation.State) == contract.ModuleActive
		}
	}
	return false
}

func (s *ModuleStore) admitCapabilityLeaseTx(ctx context.Context, tx *sql.Tx, expected contract.CapabilityLease, now time.Time) error {
	scopeJSON, err := json.Marshal(expected.Scope)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_capability_lease SET admitted_attempts = admitted_attempts + 1 WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at > ? AND credential_ref = ?`), string(expected.TenantID), expected.ID, string(scopeJSON), string(expected.CallerModuleID), string(expected.ProviderModuleID), expected.ProviderID, expected.Operation, expected.BindingRevision, expected.GrantEpoch, now.UTC().Format(capabilityLeaseTimeLayout), expected.CredentialRef)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
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
	if _, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`), string(expected.TenantID)); err != nil {
		return fmt.Errorf("fence transactional capability admission against module lifecycle: %w", err)
	}
	snapshot, err := loadModuleSnapshotTx(ctx, tx, s.Dialect, expected.TenantID)
	if err != nil {
		return err
	}
	for _, moduleID := range []contract.ModuleID{expected.CallerModuleID, expected.ProviderModuleID} {
		if !moduleLifecycleActive(snapshot, moduleID) {
			return contract.NewError(contract.CodePermissionDenied, "capability module is not active")
		}
	}
	return s.admitCapabilityLeaseTx(ctx, tx, expected, now)
}

// ValidateCapabilityLease checks that the complete durable grant still matches
// the grant pinned by the caller after a runtime restart or reload.
func (s *ModuleStore) ValidateCapabilityLease(ctx context.Context, expected contract.CapabilityLease, now time.Time) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || expected.Validate() != nil || now.IsZero() {
		return fmt.Errorf("capability lease validation identity is invalid")
	}
	var scopeJSON, caller, providerModule, provider, operation, expiresAt, state, credentialRef string
	var bindingRevision, grantEpoch uint64
	err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, `SELECT scope_json, caller_module_id, provider_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, credential_ref FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?`), string(expected.TenantID), expected.ID).Scan(&scopeJSON, &caller, &providerModule, &provider, &operation, &bindingRevision, &grantEpoch, &expiresAt, &state, &credentialRef)
	if err != nil {
		return fmt.Errorf("capability lease is unavailable")
	}
	wantScope, err := json.Marshal(expected.Scope)
	if err != nil {
		return err
	}
	wantExpiry := expected.ExpiresAt.UTC().Format(capabilityLeaseTimeLayout)
	if scopeJSON != string(wantScope) || caller != string(expected.CallerModuleID) || providerModule != string(expected.ProviderModuleID) || provider != expected.ProviderID || operation != expected.Operation || bindingRevision != expected.BindingRevision || grantEpoch != expected.GrantEpoch || expiresAt != wantExpiry || credentialRef != expected.CredentialRef || state != "active" || expiresAt <= now.UTC().Format(capabilityLeaseTimeLayout) {
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

func (s *ModuleStore) LoadTx(ctx context.Context, tx *sql.Tx, tenant contract.TenantID) (contract.ModuleSnapshot, error) {
	if s == nil || s.Dialect == nil || tx == nil || tenant == "" {
		return contract.ModuleSnapshot{}, fmt.Errorf("module snapshot transaction and tenant are required")
	}
	return loadModuleSnapshotTx(ctx, tx, s.Dialect, tenant)
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
