package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

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
	_, err := s.DB.Exec(s.SchemaStatement())
	return err
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
