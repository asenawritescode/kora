package configstore

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/db"
)

// BumpAuthorizationRevisionTx advances the tenant authorization epochs in the
// same transaction as a policy or membership mutation. Callers must have
// bootstrapped the tenant system tables first.
func BumpAuthorizationRevisionTx(tx *sql.Tx, dialect db.QueryDialect, policy, membership bool) error {
	if err := bumpAuthorizationEpochsTx(tx, dialect, policy, membership); err != nil {
		return err
	}
	if policy {
		return BumpConfigRevisionTx(tx, dialect)
	}
	return nil
}

// BumpAuthorizationRevisionAtConfigRevisionTx advances policy and tenant
// configuration revisions as one transaction, conditional on the revision
// observed by the package/config activation caller.
func BumpAuthorizationRevisionAtConfigRevisionTx(tx *sql.Tx, dialect db.QueryDialect, expected uint64) error {
	if err := bumpAuthorizationEpochsTx(tx, dialect, true, false); err != nil {
		return err
	}
	return BumpConfigRevisionAtTx(tx, dialect, expected)
}

func bumpAuthorizationEpochsTx(tx *sql.Tx, dialect db.QueryDialect, policy, membership bool) error {
	if tx == nil || dialect == nil {
		return fmt.Errorf("authorization revision update requires transaction and dialect")
	}
	if !policy && !membership {
		return nil
	}
	var sets []string
	if policy {
		sets = append(sets, "policy_revision = policy_revision + 1")
	}
	if membership {
		sets = append(sets, "membership_revision = membership_revision + 1")
	}
	query := "UPDATE _kora_auth_revision SET " + joinRevisionSets(sets) + " WHERE id = 1"
	result, err := tx.Exec(db.Rebind(dialect, query))
	if err != nil {
		return fmt.Errorf("advance authorization revision: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check authorization revision update: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("authorization revision row is missing")
	}
	return nil
}

func joinRevisionSets(sets []string) string {
	return strings.Join(sets, ", ")
}
