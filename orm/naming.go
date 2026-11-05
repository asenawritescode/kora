package orm

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// counterKeyLimit matches _kora_naming_series.prefix.
const counterKeyLimit = 32

// AllocateDocumentName returns the generated name for a new record and its
// numeric index. DocTypes with a naming series number each expanded prefix
// independently; others keep the PREFIX-NNNN default.
func AllocateDocumentName(dbTx *sql.Tx, dialect db.Dialect, dt *doctype.DocType, values map[string]any, at time.Time) (string, int64, error) {
	if dt.NamingSeries == "" {
		prefix := derivePrefix(dt.Name)
		n, err := dialect.AllocateNameNumber(dbTx, dt.Name, prefix, dt.RawTableName())
		if err != nil {
			return "", 0, err
		}
		return fmt.Sprintf("%s-%04d", prefix, n), n, nil
	}
	series, err := doctype.ParseNamingSeries(dt.NamingSeries)
	if err != nil {
		return "", 0, err
	}
	prefix, err := series.Prefix(values, at)
	if err != nil {
		return "", 0, &doctype.ValidationError{Type: "NamingSeries", Message: err.Error(), DocType: dt.Name}
	}
	seed, err := highestSeriesNumber(dbTx, dialect, dt.RawTableName(), prefix)
	if err != nil {
		return "", 0, err
	}
	n, err := dialect.AllocateSeriesNumber(dbTx, dt.Name, seriesCounterKey(prefix), seed)
	if err != nil {
		return "", 0, err
	}
	return series.Format(prefix, n), n, nil
}

// DocumentValues decodes a JSON payload for naming-series field tokens.
func DocumentValues(raw json.RawMessage) map[string]any {
	values := map[string]any{}
	_ = json.Unmarshal(raw, &values)
	return values
}

// highestSeriesNumber seeds a new counter from names already stored under the
// prefix, so adopting a series on an existing table cannot reissue a name.
func highestSeriesNumber(dbTx *sql.Tx, dialect db.Dialect, table, prefix string) (int64, error) {
	rows, err := dbTx.Query(db.Rebind(dialect, fmt.Sprintf(
		"SELECT name FROM %s WHERE name LIKE ? ESCAPE '!' ORDER BY LENGTH(name) DESC, name DESC LIMIT 20",
		dialect.QuoteIdent(table))), escapeLike(prefix)+"%")
	if err != nil {
		return 0, fmt.Errorf("reading existing series names: %w", err)
	}
	defer rows.Close()
	var highest int64
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, err
		}
		if n, ok := doctype.CounterFromName(prefix, name); ok && n > highest {
			highest = n
		}
	}
	return highest, rows.Err()
}

func escapeLike(value string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(value)
}

func seriesCounterKey(prefix string) string {
	if len(prefix) <= counterKeyLimit {
		return prefix
	}
	sum := sha256.Sum256([]byte(prefix))
	return "h:" + hex.EncodeToString(sum[:])[:counterKeyLimit-2]
}
