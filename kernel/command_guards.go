package kernel

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

// CommandGuard is a precondition checked inside the command transaction before
// any step writes. Exactly one of Lock, Exists, NotExists, Payment or Equal is set.
//
//   - lock serializes concurrent commands on one record (SELECT ... FOR UPDATE),
//     so a following not_exists check and create cannot race.
//   - exists requires at least one matching record; As binds the first match
//     for later steps as $steps.<as>.name; $steps.<as>.<field> reads one of its
//     scalar fields, which needs read access to the record.
//   - not_exists requires that no record matches.
//   - equal requires two numeric operands (a $computed or $input reference, or a
//     decimal literal) to be exactly equal, compared as big.Rat.
//
// UnlessRole, valid on exists, not_exists and equal, skips the guard when the
// verified actor holds any listed role (exact, case-sensitive match). It cannot
// be combined with As, since a skipped guard would leave the alias unbound.
type CommandGuard struct {
	Lock       *GuardLock    `yaml:"lock,omitempty" json:"lock,omitempty"`
	Exists     *GuardMatch   `yaml:"exists,omitempty" json:"exists,omitempty"`
	NotExists  *GuardMatch   `yaml:"not_exists,omitempty" json:"not_exists,omitempty"`
	Payment    *GuardPayment `yaml:"payment,omitempty" json:"payment,omitempty"`
	Equal      []string      `yaml:"equal,omitempty" json:"equal,omitempty"`
	Message    string        `yaml:"message,omitempty" json:"message,omitempty"`
	As         string        `yaml:"as,omitempty" json:"as,omitempty"`
	UnlessRole []string      `yaml:"unless_role,omitempty" json:"unless_role,omitempty"`
}

// skippedFor reports whether the guard is waived for an actor holding roles.
func (g CommandGuard) skippedFor(roles []string) bool {
	for _, listed := range g.UnlessRole {
		for _, held := range roles {
			if listed == held {
				return true
			}
		}
	}
	return false
}

func (c *CommandResource) validateUnlessRole(index int, guard CommandGuard) error {
	if guard.UnlessRole == nil {
		return nil
	}
	if guard.Lock != nil || guard.Payment != nil {
		return fmt.Errorf("command %q: require %d unless_role is only valid on exists, not_exists and equal", c.FullName(), index)
	}
	if guard.As != "" {
		return fmt.Errorf("command %q: require %d unless_role cannot be combined with an alias", c.FullName(), index)
	}
	if len(guard.UnlessRole) == 0 {
		return fmt.Errorf("command %q: require %d unless_role needs at least one role", c.FullName(), index)
	}
	for _, role := range guard.UnlessRole {
		if strings.TrimSpace(role) == "" {
			return fmt.Errorf("command %q: require %d unless_role has an empty role name", c.FullName(), index)
		}
	}
	return nil
}

// GuardLock names one record to lock, usually "$input.field".
type GuardLock struct {
	Record string `yaml:"record" json:"record"`
	Name   string `yaml:"name" json:"name"`
}

// GuardMatch is an equality match over declared fields. Values may be literals
// or $input / $steps references.
type GuardMatch struct {
	Record string            `yaml:"record" json:"record"`
	Where  map[string]string `yaml:"where" json:"where"`
}

func (c *CommandResource) validateGuards(aliases map[string]bool) error {
	payments := 0
	for i, guard := range c.Require {
		if err := c.validateGuardRefs(i, guard, aliases); err != nil {
			return err
		}
		set := 0
		for _, present := range []bool{guard.Lock != nil, guard.Exists != nil, guard.NotExists != nil, guard.Payment != nil, len(guard.Equal) > 0} {
			if present {
				set++
			}
		}
		if set != 1 {
			return fmt.Errorf("command %q: require %d must set exactly one of lock, exists, not_exists, payment or equal", c.FullName(), i)
		}
		if err := c.validateUnlessRole(i, guard); err != nil {
			return err
		}
		switch {
		case guard.Lock != nil:
			if guard.Lock.Record == "" || guard.Lock.Name == "" {
				return fmt.Errorf("command %q: require %d lock needs record and name", c.FullName(), i)
			}
		case len(guard.Equal) > 0:
			if err := c.validateEqualGuard(i, guard.Equal); err != nil {
				return err
			}
		case guard.Payment != nil:
			if payments++; payments > 1 {
				return fmt.Errorf("command %q: at most one payment guard is allowed", c.FullName())
			}
			if err := c.validatePaymentGuard(i, guard.Payment); err != nil {
				return err
			}
		default:
			match := guard.Exists
			if match == nil {
				match = guard.NotExists
			}
			if match.Record == "" || len(match.Where) == 0 {
				return fmt.Errorf("command %q: require %d needs record and where", c.FullName(), i)
			}
		}
		if guard.As != "" {
			if guard.Exists == nil {
				return fmt.Errorf("command %q: require %d alias is only valid on exists", c.FullName(), i)
			}
			if !stepAliasPattern.MatchString(guard.As) || aliases[guard.As] {
				return fmt.Errorf("command %q: require %d alias %q must be a unique lower-case identifier", c.FullName(), i, guard.As)
			}
			aliases[guard.As] = true
		}
	}
	return nil
}

func (k *Kernel) checkCommandGuards(dbTx *sql.Tx, opID string, txMgr *orm.TxManager, reg *doctype.Registry, op Operation, cmd *CommandResource, env refEnv) *contract.Error {
	for i, guard := range cmd.Require {
		if guard.skippedFor(op.Context.Roles) {
			continue
		}
		if guard.Lock != nil {
			if cerr := k.lockGuardRecord(dbTx, reg, guard, env, i); cerr != nil {
				return cerr
			}
			continue
		}
		if len(guard.Equal) > 0 {
			if cerr := checkEqualGuard(guard, env, i); cerr != nil {
				return cerr
			}
			continue
		}
		if guard.Payment != nil {
			if cerr := k.consumePaymentGuard(dbTx, op.Context.Site, opID, guard.Payment, cmd.Input.Record, env, i); cerr != nil {
				return cerr
			}
			continue
		}
		match, wantExists := guard.Exists, true
		if match == nil {
			match, wantExists = guard.NotExists, false
		}
		dt := reg.Get(match.Record)
		if dt == nil {
			return contract.NewError(contract.CodeNotFound, fmt.Sprintf("require %d: record %q not found", i, match.Record))
		}
		filters, cerr := guardFilters(dt, match, env, i)
		if cerr != nil {
			return cerr
		}
		found, exists, err := txMgr.FirstMatchingNameLocked(dt, filters, scopeForOperation(reg, op, dt.Name, "read"))
		if err != nil {
			return contract.NewError(contract.CodeDependencyUnavailable, fmt.Sprintf("require %d: check failed", i))
		}
		if exists != wantExists {
			message := guard.Message
			if message == "" {
				message = fmt.Sprintf("precondition %d on %s was not met", i, dt.Name)
			}
			return contract.NewError(contract.CodeConflict, message)
		}
		if guard.As != "" {
			env.steps[guard.As] = found
		}
	}
	return nil
}

// guardFilters builds the ORM filter list. Field names are checked against the
// DocType, and values are bound by the ORM, never interpolated.
func guardFilters(dt *doctype.DocType, match *GuardMatch, env refEnv, index int) (string, *contract.Error) {
	fields := make([]string, 0, len(match.Where))
	for field := range match.Where {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	filters := make([][]any, 0, len(fields))
	for _, field := range fields {
		if field != "name" {
			if f := dt.GetField(field); f == nil || f.Fieldtype == "Table" {
				return "", contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("require %d: %s has no field %q", index, dt.Name, field))
			}
		}
		value, err := resolveEnvRef(match.Where[field], env)
		if err != nil {
			return "", contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("require %d: %v", index, err))
		}
		filters = append(filters, []any{field, "=", value})
	}
	raw, _ := json.Marshal(filters)
	return string(raw), nil
}

func (k *Kernel) lockGuardRecord(dbTx *sql.Tx, reg *doctype.Registry, guard CommandGuard, env refEnv, index int) *contract.Error {
	dt := reg.Get(guard.Lock.Record)
	if dt == nil {
		return contract.NewError(contract.CodeNotFound, fmt.Sprintf("require %d: record %q not found", index, guard.Lock.Record))
	}
	name, err := resolveEnvStringRef(guard.Lock.Name, env)
	if err != nil || name == "" {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("require %d: lock name is required", index))
	}
	dialect := k.dialect()
	query := fmt.Sprintf("SELECT name FROM %s WHERE name = ?", dialect.QuoteIdent(dt.RawTableName()))
	if named, ok := dialect.(interface{ DriverName() string }); !ok || named.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	var locked string
	if err := dbTx.QueryRow(db.Rebind(dialect, query), name).Scan(&locked); err != nil {
		if err == sql.ErrNoRows {
			message := guard.Message
			if message == "" {
				message = fmt.Sprintf("%s %q not found", dt.Name, name)
			}
			return contract.NewError(contract.CodeNotFound, message)
		}
		return contract.NewError(contract.CodeDependencyUnavailable, fmt.Sprintf("require %d: lock failed", index))
	}
	return nil
}

func (c *CommandResource) validateGuardRefs(index int, guard CommandGuard, aliases map[string]bool) error {
	var refs []string
	if guard.Lock != nil {
		refs = append(refs, guard.Lock.Name)
	}
	for _, match := range []*GuardMatch{guard.Exists, guard.NotExists} {
		if match != nil {
			for _, ref := range match.Where {
				refs = append(refs, ref)
			}
		}
	}
	for _, ref := range refs {
		if strings.HasPrefix(ref, "$item.") {
			return fmt.Errorf("command %q: require %d cannot use %q", c.FullName(), index, ref)
		}
		if strings.HasPrefix(ref, "$steps.") {
			alias, _, ok := stepFieldRef(ref)
			if !ok || !aliases[alias] {
				return fmt.Errorf("command %q: require %d reference %q must name an earlier require alias", c.FullName(), index, ref)
			}
		}
	}
	return nil
}
