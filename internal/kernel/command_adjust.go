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

// FieldAdjust changes one numeric field relative to its stored value. It takes
// one of two forms:
//
//   - plain: exactly one of Increase or Decrease is set to the amount reference,
//     e.g. `{ increase: $input.amount }`.
//   - signed: By is the amount reference and SignFrom names a value that picks
//     the direction. A value equal to IncreaseWhen adds the amount and a value
//     equal to DecreaseWhen subtracts it, e.g. `{ by: $input.amount, sign_from:
//     $input.direction, increase_when: In, decrease_when: Out }`. When both are
//     set, any other value fails the command. When only one is set, a matching
//     value moves the field and any other value leaves it unchanged, e.g.
//     `{ by: "1", sign_from: $input.direction, increase_when: Out }` counts Outs.
//
// Min, when set, is a floor the result may not fall below. Arithmetic is exact
// decimal, never float.
type FieldAdjust struct {
	Increase     string `yaml:"increase,omitempty" json:"increase,omitempty"`
	Decrease     string `yaml:"decrease,omitempty" json:"decrease,omitempty"`
	Min          string `yaml:"min,omitempty" json:"min,omitempty"`
	By           string `yaml:"by,omitempty" json:"by,omitempty"`
	SignFrom     string `yaml:"sign_from,omitempty" json:"sign_from,omitempty"`
	IncreaseWhen string `yaml:"increase_when,omitempty" json:"increase_when,omitempty"`
	DecreaseWhen string `yaml:"decrease_when,omitempty" json:"decrease_when,omitempty"`
}

// signed reports whether the change uses the signed form.
func (a FieldAdjust) signed() bool {
	return a.By != "" || a.SignFrom != "" || a.IncreaseWhen != "" || a.DecreaseWhen != ""
}

func (a FieldAdjust) validate() error {
	if a.Min != "" {
		if _, ok := new(big.Rat).SetString(a.Min); !ok {
			return fmt.Errorf("adjust min %q must be a number", a.Min)
		}
	}
	if !a.signed() {
		if (a.Increase == "") == (a.Decrease == "") {
			return fmt.Errorf("adjust needs exactly one of increase or decrease")
		}
		return nil
	}
	if a.Increase != "" || a.Decrease != "" {
		return fmt.Errorf("signed adjust cannot also set increase or decrease")
	}
	if a.By == "" || a.SignFrom == "" {
		return fmt.Errorf("signed adjust needs by and sign_from")
	}
	if a.IncreaseWhen == "" && a.DecreaseWhen == "" {
		return fmt.Errorf("signed adjust needs increase_when or decrease_when")
	}
	if a.IncreaseWhen != "" && a.IncreaseWhen == a.DecreaseWhen {
		return fmt.Errorf("signed adjust needs two different direction values")
	}
	return nil
}

// amountRef returns the amount reference, whether the change adds to the stored
// value, and whether it applies at all. A signed change reads SignFrom. When
// both directions are set, a value matching neither fails the command. When
// only one is set, a value that does not match leaves the field unchanged.
func (a FieldAdjust) amountRef(env refEnv, index int) (ref string, increase, applies bool, cerr *contract.Error) {
	if !a.signed() {
		if a.Increase != "" {
			return a.Increase, true, true, nil
		}
		return a.Decrease, false, true, nil
	}
	direction, err := resolveEnvRef(a.SignFrom, env)
	if err != nil {
		return "", false, false, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", index, err))
	}
	value := fmt.Sprint(direction)
	if a.IncreaseWhen != "" && value == a.IncreaseWhen {
		return a.By, true, true, nil
	}
	if a.DecreaseWhen != "" && value == a.DecreaseWhen {
		return a.By, false, true, nil
	}
	if a.IncreaseWhen != "" && a.DecreaseWhen != "" {
		return "", false, false, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %s must be %q or %q", index, a.SignFrom, a.IncreaseWhen, a.DecreaseWhen))
	}
	return "", false, false, nil
}

// stepEnv is a command step's reference environment and its position, which
// error messages quote.
type stepEnv struct {
	env   refEnv
	index int
}

// resolveUpdateTarget returns the record an update step changes: by name, or
// the single record matching where.
func resolveUpdateTarget(txMgr *orm.TxManager, dt *doctype.DocType, step *StepUpdate, at stepEnv, scope policy.RowScope) (string, *contract.Error) {
	env, index := at.env, at.index
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
func applyAdjustments(dt *doctype.DocType, oldDoc, doc *doctype.Document, adjust map[string]FieldAdjust, at stepEnv) *contract.Error {
	env, index := at.env, at.index
	for field, change := range adjust {
		definition := dt.GetField(field)
		if definition == nil || (definition.Fieldtype != "Int" && definition.Fieldtype != "Float" && definition.Fieldtype != "Currency" && definition.Fieldtype != "Percent") {
			return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %s.%s is not a numeric field", index, dt.Name, field))
		}
		ref, increase, applies, cerr := change.amountRef(env, index)
		if cerr != nil {
			return cerr
		}
		if !applies {
			continue
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
		next := moveExact(current, delta, increase)
		if change.Min != "" {
			floor, _ := new(big.Rat).SetString(change.Min)
			if next.Cmp(floor) < 0 {
				return contract.NewError(contract.CodeConflict, fmt.Sprintf("%s %s would fall below %s", dt.Name, field, change.Min))
			}
		}
		doc.Set(field, storedNumber(definition.Fieldtype, next))
	}
	return nil
}

// moveExact returns current plus delta when increase is set, else current minus
// delta, in exact decimal.
func moveExact(current, delta *big.Rat, increase bool) *big.Rat {
	next := new(big.Rat)
	if increase {
		return next.Add(current, delta)
	}
	return next.Sub(current, delta)
}

// storedNumber converts an exact result to the value stored for a field of the
// given type: an integer for Int, otherwise a trimmed decimal string.
func storedNumber(fieldtype string, next *big.Rat) any {
	if fieldtype == "Int" {
		return next.Num().Int64()
	}
	return strings.TrimRight(strings.TrimRight(next.FloatString(9), "0"), ".")
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
func ensureExisting(txMgr *orm.TxManager, reg *doctype.Registry, step *StepEnsure, at stepEnv, scope policy.RowScope) (string, *contract.Error) {
	env, index := at.env, at.index
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
