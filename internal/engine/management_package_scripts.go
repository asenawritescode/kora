package engine

import (
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
)

// checkPackageScripts runs the script ownership and binding checks of a package
// against the composition it would install. It only reads. It runs in the planning
// step, before the activation journal and any DDL, so a refusal is a plan refusal.
func checkPackageScripts(tenant packageTenantDB, authority packageAuthority, scripts []*doctype.ScriptSnapshot) error {
	candidate := authority.installed
	candidate.Packages = withRelease(authority.installed.Packages, authority.release)
	if err := configstore.CheckPendingScriptBindings(candidate, scripts); err != nil {
		return err
	}
	return configstore.CheckScriptOwnership(tenant.conn, tenant.dialect, scripts)
}

// withRelease returns packages with release set in place of any record of its release.
func withRelease(packages []contract.InstalledPackage, release contract.InstalledPackage) []contract.InstalledPackage {
	out := make([]contract.InstalledPackage, 0, len(packages)+1)
	for _, pkg := range packages {
		if pkg.ReleaseID != release.ReleaseID {
			out = append(out, pkg)
		}
	}
	return append(out, release)
}

// docTypeNames lists the DocType names of a snapshot, for the package record.
func docTypeNames(docTypes []*doctype.DocType) []string {
	names := make([]string, 0, len(docTypes))
	for _, dt := range docTypes {
		if dt != nil {
			names = append(names, dt.Name)
		}
	}
	return names
}
