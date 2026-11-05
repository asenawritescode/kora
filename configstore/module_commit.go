package configstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

// CommitModulesTx merges modules into the tenant module snapshot inside tx:
// descriptors, active installations, ownership, and a CAS on the snapshot
// revision. Callers must run ModuleStore.EnsureSchema before opening tx.
func (s *Store) CommitModulesTx(ctx context.Context, tx *sql.Tx, dialect db.Dialect, tenantID contract.TenantID, modules []contract.ModuleDescriptor, digest string, expectedRevision uint64, providerSelections []contract.ProviderSelection, packageConstraints []contract.InstalledPackage) error {
	if _, err := contract.ParseTenantID(string(tenantID)); err != nil {
		return fmt.Errorf("module snapshot tenant identity is invalid: %w", err)
	}
	moduleStore := NewModuleStore(s.DB, dialect)
	existing, err := loadModuleSnapshotTx(ctx, tx, dialect, tenantID)
	if err != nil {
		return err
	}
	if existing.Revision != expectedRevision {
		return fmt.Errorf("module snapshot revision conflict: expected %d, actual %d", expectedRevision, existing.Revision)
	}
	ownership, err := replaceModuleOwnership(existing.Ownership, tenantID, modules)
	if err != nil {
		return err
	}
	selections := existing.ProviderSelections
	if providerSelections != nil {
		selections = providerSelections
	}
	candidate := contract.ModuleSnapshot{TenantID: tenantID, Modules: mergeInstalledDescriptors(existing.Modules, modules), Packages: mergeInstalledPackages(existing.Packages, packageConstraints), Installations: replaceModuleInstallations(existing.Installations, tenantID, modules, digest), Ownership: ownership, ProviderSelections: append([]contract.ProviderSelection(nil), selections...)}
	if err := validateModuleSnapshot(candidate); err != nil {
		return err
	}
	_, err = moduleStore.CommitActivationTx(ctx, tx, expectedRevision, candidate)
	return err
}
