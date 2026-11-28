package configstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// scriptRowQuerier is the read that both *sql.DB and *sql.Tx provide.
type scriptRowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// LegacyScriptName is the unnamespaced _kora_script name a document-event hook
// had before pack ownership: <DocType with underscores>_<event>.
func LegacyScriptName(docType, event string) string {
	return strings.ReplaceAll(docType, " ", "_") + "_" + event
}

// legacyRowName is the unnamespaced row an owned document-event hook replaces, or
// "" when the script is not a document-event hook.
func legacyRowName(ss *doctype.ScriptSnapshot) string {
	if ss.ScriptType != "doc_event" || ss.DocType == "" {
		return ""
	}
	return LegacyScriptName(ss.DocType, ss.Event)
}

// CheckScriptOwnership refuses an owned script that would take a row another pack
// owns, or an unowned row whose stored body differs from the script. An unowned row
// of the script's own name, or the unnamespaced legacy row of its document-event
// hook, is adoptable when its stored body is empty or hashes to the script's
// ScriptHash. A script with an empty OwnerPack (written before ownership) never
// changes an owner.
func CheckScriptOwnership(q scriptRowQuerier, dialect db.QueryDialect, scripts []*doctype.ScriptSnapshot) error {
	for _, ss := range scripts {
		if ss == nil || ss.OwnerPack == "" {
			continue
		}
		if err := checkScriptRow(q, dialect, ss, ss.Name, true); err != nil {
			return err
		}
		if legacy := legacyRowName(ss); legacy != "" {
			if err := checkScriptRow(q, dialect, ss, legacy, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkScriptRow checks one stored row of name against the owned script ss. own is
// true for the row the script itself writes; another pack's legacy row is left for
// the retire pass and is not an error.
func checkScriptRow(q scriptRowQuerier, dialect db.QueryDialect, ss *doctype.ScriptSnapshot, name string, own bool) error {
	var owner, body string
	err := q.QueryRow(db.Rebind(dialect, `SELECT owner_pack, script FROM _kora_script WHERE name = ?`), name).Scan(&owner, &body)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("reading script owner: %w", err)
	case owner == ss.OwnerPack:
		return nil
	case owner != "":
		if own {
			return refuse("pack.script_owned_elsewhere", "script %q is owned by %q and cannot be taken by %q; installed scripts without an owner need an explicit decision", name, owner, ss.OwnerPack)
		}
		return nil
	case body == "" || sameScriptContent(body, ss.ScriptHash):
		return nil
	default:
		return refuse("pack.script_unowned_mismatch", "row %q of pack %q holds content that differs from the pack's script; the row is left as it is", name, ss.OwnerPack)
	}
}

// adoptLegacyScriptRowTx gives an unowned legacy row of an owned document-event hook
// to the hook's pack. CheckScriptOwnership has already accepted the row's content.
// The retire pass that follows deactivates the row behind the namespaced hook.
func adoptLegacyScriptRowTx(tx *sql.Tx, ss *doctype.ScriptSnapshot, site string, dialect db.QueryDialect) error {
	legacy := legacyRowName(ss)
	if ss.OwnerPack == "" || legacy == "" {
		return nil
	}
	var owner, body string
	err := tx.QueryRow(db.Rebind(dialect, `SELECT owner_pack, script FROM _kora_script WHERE name = ? AND site = ?`), legacy, site).Scan(&owner, &body)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != "") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading legacy script %s: %w", legacy, err)
	}
	res, err := tx.Exec(db.Rebind(dialect, `UPDATE _kora_script SET owner_pack = ? WHERE name = ? AND site = ? AND owner_pack = ''`), ss.OwnerPack, legacy, site)
	if err != nil {
		return fmt.Errorf("adopting legacy script %s: %w", legacy, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("adopting legacy script %s: %w", legacy, err)
	}
	if n != 1 {
		return fmt.Errorf("adopting legacy script %s: row changed concurrently (rows %d)", legacy, n)
	}
	slog.Info("adopted unowned script row", "row", legacy, "pack", ss.OwnerPack, "body_empty", body == "")
	return nil
}

// sameScriptContent reports whether body is the content whose SHA-256 is hash.
func sameScriptContent(body, hash string) bool {
	sum := sha256.Sum256([]byte(body))
	return hash != "" && hex.EncodeToString(sum[:]) == hash
}
