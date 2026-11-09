package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// HashExtensionAccessToken returns the stored verifier for an extension or
// managed channel bearer token. Tokens are 256-bit random values, so an
// unsalted SHA-256 digest is sufficient and keeps lookups indexable.
func HashExtensionAccessToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type extensionTokenRow struct {
	site, name, token string
}

// BackfillExtensionTokenHashes replaces plaintext tokens stored before
// access_token_hash existed with their verifier. It is idempotent.
func BackfillExtensionTokenHashes(ctx context.Context, database *sql.DB, dialect kdb.QueryDialect) error {
	rows, err := database.QueryContext(ctx, `SELECT site, name, access_token FROM _kora_extension WHERE access_token <> ''`)
	if err != nil {
		return fmt.Errorf("list extension tokens without verifier: %w", err)
	}
	var pending []extensionTokenRow
	for rows.Next() {
		var row extensionTokenRow
		if err := rows.Scan(&row.site, &row.name, &row.token); err != nil {
			rows.Close()
			return fmt.Errorf("read extension token row: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close extension token rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate extension token rows: %w", err)
	}
	update := kdb.Rebind(dialect, `UPDATE _kora_extension SET access_token_hash = ?, access_token = '' WHERE site = ? AND name = ? AND access_token = ?`)
	for _, row := range pending {
		if _, err := database.ExecContext(ctx, update, HashExtensionAccessToken(row.token), row.site, row.name, row.token); err != nil {
			return fmt.Errorf("backfill extension token verifier for %s: %w", row.name, err)
		}
	}
	return nil
}
