package kernel

import (
	"database/sql"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// FieldAdjust changes one numeric field relative to its stored value. Exactly
// one of Increase or Decrease is set; Min, when set, is a floor the result may
// not fall below. Arithmetic is exact decimal, never float.
type FieldAdjust struct {
	Increase string `yaml:"increase,omitempty" json:"increase,omitempty"`
	Decrease string `yaml:"decrease,omitempty" json:"decrease,omitempty"`
	Min      string `yaml:"min,omitempty" json:"min,omitempty"`
}

func (a FieldAdjust) validate() error {
	if (a.Increase == "") == (a.Decrease == "") {
		return fmt.Errorf("adjust needs exactly one of increase or decrease")
	}
	if a.Min != "" {
		if _, ok := new(big.Rat).SetString(a.Min); !ok {
			return fmt.Errorf("adjust min %q must be a number", a.Min)
		}
	}
	return nil
}

// resolveUpdateTarget returns the record an update step changes: by name, or
// the single record matching where.
func resolveUpdateTarget(txMgr *orm.TxManager, dt *doctype.DocType, step *StepUpdate, env refEnv, scope policy.RowScope, index int) (string, *contract.Error) {
	if step.Name != "" {
		name, err := resolveEnvStringRef(step.Name, env)
		if err != nil || name == "" {
			return "", contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", index, err))
		}
		return name, nil
	}
	filters, cerr := guardFilters(dt, &GuardMatch{Record: step.Record, Where: step.Where}, env, index)
	if cerr != nil {
		return "", cerr
	}
	name, found, err := txMgr.FirstMatchingNameLocked(dt, filters, scope)
	if err != nil {
		return "", contract.NewError(contract.CodeDependencyUnavailable, fmt.Sprintf("step %d: lookup failed", index))
	}
	if !found {
		return "", contract.NewError(contract.CodeNotFound, fmt.Sprintf("step %d: no %s matches", index, dt.Name))
	}
	return name, nil
}

// lockRecordRow holds the row for the rest of the transaction so concurrent
// adjustments of the same record apply one after another.
func lockRecordRow(dbTx *sql.Tx, dialect db.Dialect, dt *doctype.DocType, name string) error {
	query := fmt.Sprintf("SELECT name FROM %s WHERE name = ?", dialect.QuoteIdent(dt.RawTableName()))
	if named, ok := dialect.(interface{ DriverName() string }); !ok || named.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	var locked string
	return dbTx.QueryRow(db.Rebind(dialect, query), name).Scan(&locked)
}

// applyAdjustments computes each adjusted value from the stored one.
func applyAdjustments(dt *doctype.DocType, oldDoc, doc *doctype.Document, adjust map[string]FieldAdjust, env refEnv, index int) *contract.Error {
	for field, change := range adjust {
		definition := dt.GetField(field)
		if definition == nil || (definition.Fieldtype != "Int" && definition.Fieldtype != "Float" && definition.Fieldtype != "Currency" && definition.Fieldtype != "Percent") {
			return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %s.%s is not a numeric field", index, dt.Name, field))
		}
		ref := change.Increase
		if ref == "" {
			ref = change.Decrease
		}
		raw, err := resolveEnvRef(ref, env)
		if err != nil {
			return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", index, err))
		}
		delta, ok := exactNumber(raw)
		if !ok || delta.Sign() < 0 || definition.Fieldtype == "Int" && !delta.IsInt() {
			return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %s change must be a non-negative %s", index, field, strings.ToLower(definition.Fieldtype)))
		}
		current, ok := exactNumber(oldDoc.Get(field))
		if !ok {
			current = new(big.Rat)
		}
		next := new(big.Rat)
		if change.Increase != "" {
			next.Add(current, delta)
		} else {
			next.Sub(current, delta)
		}
		if change.Min != "" {
			floor, _ := new(big.Rat).SetString(change.Min)
			if next.Cmp(floor) < 0 {
				return contract.NewError(contract.CodeConflict, fmt.Sprintf("%s %s would fall below %s", dt.Name, field, change.Min))
			}
		}
		if definition.Fieldtype == "Int" {
			doc.Set(field, next.Num().Int64())
		} else {
			doc.Set(field, strings.TrimRight(strings.TrimRight(next.FloatString(9), "0"), "."))
		}
	}
	return nil
}

// exactNumber reads a stored or supplied value as an exact decimal. Floats go
// through their shortest round-trip form so 0.1 stays 0.1.
func exactNumber(value any) (*big.Rat, bool) {
	var text string
	switch v := value.(type) {
	case nil:
		return nil, false
	case float64:
		text = strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		text = strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int:
		text = strconv.Itoa(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	case string:
		text = strings.TrimSpace(v)
	case []byte:
		text = strings.TrimSpace(string(v))
	default:
		text = fmt.Sprint(v)
	}
	return new(big.Rat).SetString(text)
}

// ensureExisting returns the name of the record an ensure step finds, or ""
// when none matches and the step must create it.
func ensureExisting(txMgr *orm.TxManager, reg *doctype.Registry, step *StepEnsure, env refEnv, scope policy.RowScope, index int) (string, *contract.Error) {
	dt := reg.Get(step.Record)
	if dt == nil {
		return "", contract.NewError(contract.CodeNotFound, fmt.Sprintf("step %d: record %q not found", index, step.Record))
	}
	filters, cerr := guardFilters(dt, &GuardMatch{Record: step.Record, Where: step.Where}, env, index)
	if cerr != nil {
		return "", cerr
	}
	name, _, err := txMgr.FirstMatchingNameLocked(dt, filters, scope)
	if err != nil {
		return "", contract.NewError(contract.CodeDependencyUnavailable, fmt.Sprintf("step %d: lookup failed", index))
	}
	return name, nil
}
