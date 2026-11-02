package configstore

import (
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

func replaceModuleOwnership(existing []contract.ResourceOwnership, tenant contract.TenantID, modules []contract.ModuleDescriptor) []contract.ResourceOwnership {
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
				continue // activation validation reports the conflicting owner before persistence
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
	return out
}

// ModuleStore persists the authoritative module ownership snapshot in the
// tenant database. The activation caller commits it in the same transaction
// as the configuration snapshot.
type ModuleStore struct {
	DB      *sql.DB
	Dialect db.QueryDialect
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
	if tenant == "" {
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
	return snapshot, nil
}

func (s *ModuleStore) CommitActivation(tx *sql.Tx, expectedRevision uint64, candidate contract.ModuleSnapshot) (uint64, error) {
	if tx == nil || candidate.TenantID == "" {
		return 0, fmt.Errorf("module activation requires transaction and tenant")
	}
	if err := validateModuleSnapshot(candidate); err != nil {
		return 0, err
	}
	current, err := loadModuleSnapshotTx(tx, s.Dialect, candidate.TenantID)
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
		_, err = tx.Exec(db.Rebind(s.Dialect, `INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)`), string(candidate.TenantID), candidate.Revision, string(payload))
	} else {
		var result sql.Result
		result, err = tx.Exec(db.Rebind(s.Dialect, `UPDATE _kora_module_snapshot SET revision = ?, snapshot_json = ? WHERE tenant_id = ? AND revision = ?`), candidate.Revision, string(payload), string(candidate.TenantID), expectedRevision)
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

func (s *ModuleStore) CommitActivationTx(tx *sql.Tx, expectedRevision uint64, candidate contract.ModuleSnapshot) (uint64, error) {
	return s.CommitActivation(tx, expectedRevision, candidate)
}

func loadModuleSnapshotTx(tx *sql.Tx, dialect db.QueryDialect, tenant contract.TenantID) (contract.ModuleSnapshot, error) {
	var revision uint64
	var payload string
	err := tx.QueryRow(db.Rebind(dialect, `SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`), string(tenant)).Scan(&revision, &payload)
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
	return nil
}

var _ contract.ModuleStore = (*ModuleStore)(nil)
