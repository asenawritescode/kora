package configstore

import (
	"database/sql"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// ScriptRefusal is a snapshot's script rows that cannot be written. Code is the
// stable pack.* code; Error is "code: detail", the text the import path reports.
type ScriptRefusal struct {
	Code   string
	detail string
}

func (e *ScriptRefusal) Error() string { return e.Code + ": " + e.detail }

// refuse builds the ScriptRefusal for code with a formatted message.
func refuse(code, format string, args ...any) error {
	return &ScriptRefusal{Code: code, detail: fmt.Sprintf(format, args...)}
}

// CheckSnapshotScripts is the planning check of a snapshot's script rows, run before
// any DDL or journal row is written. It refuses an owned script with no body, then
// an owned script that would take another pack's row, then pending bindings against
// the packs the tenant has installed (a no-op without pending bindings). A refusal is
// a *ScriptRefusal.
func CheckSnapshotScripts(database *sql.DB, dialect db.Dialect, tenant contract.TenantID, scripts []*doctype.ScriptSnapshot) error {
	if err := checkOwnedScriptBodies(scripts); err != nil {
		return err
	}
	if err := CheckScriptOwnership(database, dialect, scripts); err != nil {
		return err
	}
	if !HasPendingScriptBindings(scripts) {
		return nil
	}
	installedStore := NewModuleStore(database, dialect)
	if err := installedStore.EnsureSchema(); err != nil {
		return fmt.Errorf("ensuring module snapshot schema: %w", err)
	}
	installed, err := installedStore.Load(tenant)
	if err != nil {
		return fmt.Errorf("loading installed packs for script bindings: %w", err)
	}
	return CheckPendingScriptBindings(installed, scripts)
}

// checkOwnedScriptBodies refuses an owned script whose body is empty. Versions
// written before the import path bound script bodies hold such scripts; writing one
// would blank the live row and the retire pass would then turn the legacy row off.
func checkOwnedScriptBodies(scripts []*doctype.ScriptSnapshot) error {
	for _, ss := range scripts {
		if ss != nil && ss.OwnerPack != "" && ss.Source == "" {
			return refuse("pack.script_body_missing", "script %q of pack %q has no body in this version; re-import the pack to rebuild the version", ss.Name, ss.OwnerPack)
		}
	}
	return nil
}
