package configstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// HasPendingScriptBindings reports whether any script binds a DocType that only an installed pack can define.
func HasPendingScriptBindings(scripts []*doctype.ScriptSnapshot) bool {
	for _, ss := range scripts {
		if ss != nil && ss.PendingBinding {
			return true
		}
	}
	return false
}

// CheckPendingScriptBindings resolves pending pack script bindings against the
// installed modules: a binding is accepted when a pack in the closure of the
// owner's requires defines the DocType (owned in the module ownership index).
// Installed requires come from the installed package records.
func CheckPendingScriptBindings(installed contract.ModuleSnapshot, scripts []*doctype.ScriptSnapshot) error {
	defined := definedDocTypes(installed)
	requires := requiresByPackage(installed)
	for _, ss := range scripts {
		if ss == nil || !ss.PendingBinding {
			continue
		}
		if !closureDefines(ss.OwnerRequires, ss.DocType, defined, requires) {
			return fmt.Errorf("pack.script_binding_undefined: scripts/%s/%s.js in pack %q binds DocType %q, which no installed pack it requires defines", ss.DocType, ss.Event, ss.OwnerPack, ss.DocType)
		}
	}
	return nil
}

// definedDocTypes maps each installed pack to the DocTypes it defines: the names in
// its package record, and the tenant modules its ownership index attributes to it.
// A record written before the names were recorded contributes through the index only.
func definedDocTypes(installed contract.ModuleSnapshot) map[string]map[string]bool {
	defined := map[string]map[string]bool{}
	add := func(pack, name string) {
		if defined[pack] == nil {
			defined[pack] = map[string]bool{}
		}
		defined[pack][name] = true
	}
	for _, owner := range installed.Ownership {
		if owner.Resource.Namespace != "tenant" || !strings.HasSuffix(string(owner.OwnerModuleID), ".core") {
			continue
		}
		add(strings.TrimSuffix(string(owner.OwnerModuleID), ".core"), owner.Resource.Name)
	}
	for _, pkg := range installed.Packages {
		for _, name := range pkg.DocTypes {
			add(pkg.PackageID, name)
		}
	}
	return defined
}

// requiresByPackage maps each installed package to the packages its record requires.
func requiresByPackage(installed contract.ModuleSnapshot) map[string][]string {
	requires := map[string][]string{}
	for _, pkg := range installed.Packages {
		for _, ref := range pkg.Requires {
			requires[pkg.PackageID] = append(requires[pkg.PackageID], ref.PackageID)
		}
	}
	return requires
}

// closureDefines walks installed requires from start and reports whether a reached pack defines docType.
func closureDefines(start []string, docType string, defined map[string]map[string]bool, requires map[string][]string) bool {
	seen := map[string]bool{}
	queue := append([]string(nil), start...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true
		if defined[next][docType] {
			return true
		}
		queue = append(queue, requires[next]...)
	}
	return false
}

// checkPendingScriptBindingsTx loads the tenant's installed modules inside tx and resolves pending bindings.
func checkPendingScriptBindingsTx(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect, tenant contract.TenantID, scripts []*doctype.ScriptSnapshot) error {
	if !HasPendingScriptBindings(scripts) {
		return nil
	}
	installed, err := loadModuleSnapshotTx(ctx, tx, dialect, tenant)
	if err != nil {
		return fmt.Errorf("loading installed packs for script bindings: %w", err)
	}
	return CheckPendingScriptBindings(installed, scripts)
}
