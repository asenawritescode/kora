package configstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

// CheckScriptOwnership refuses an owned script that would take a row another pack
// owns. An unowned row of the same name is adopted only when its stored content
// hashes to the script's ScriptHash; otherwise the import is refused. A script with
// an empty OwnerPack (written before ownership) never changes an owner.
func CheckScriptOwnership(q scriptRowQuerier, dialect db.QueryDialect, scripts []*doctype.ScriptSnapshot) error {
	for _, ss := range scripts {
		if ss == nil || ss.OwnerPack == "" {
			continue
		}
		var owner, body string
		err := q.QueryRow(db.Rebind(dialect, `SELECT owner_pack, script FROM _kora_script WHERE name = ?`), ss.Name).Scan(&owner, &body)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("reading script owner: %w", err)
		case owner == ss.OwnerPack:
		case owner != "":
			return fmt.Errorf("pack.script_owned_elsewhere: script %q is owned by %q and cannot be taken by %q; installed scripts without an owner need an explicit decision", ss.Name, owner, ss.OwnerPack)
		case !sameScriptContent(body, ss.ScriptHash):
			return fmt.Errorf("pack.script_unowned_mismatch: script %q of pack %q matches no installed content of the unowned row of that name; the row is left as it is", ss.Name, ss.OwnerPack)
		}
	}
	return nil
}

// sameScriptContent reports whether body is the content whose SHA-256 is hash.
func sameScriptContent(body, hash string) bool {
	sum := sha256.Sum256([]byte(body))
	return hash != "" && hex.EncodeToString(sum[:]) == hash
}
