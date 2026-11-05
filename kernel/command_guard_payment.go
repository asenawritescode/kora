package kernel

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

// GuardPayment requires every payment intent in a list to be confirmed, to
// match the stated amount and currency, and to be unused. The intent is then
// marked consumed by the executing operation, in the command transaction.
type GuardPayment struct {
	ForEach   string `yaml:"for_each" json:"for_each"`
	Reference string `yaml:"reference" json:"reference"`
	Amount    string `yaml:"amount" json:"amount"`
	Currency  string `yaml:"currency" json:"currency"`
}

func (c *CommandResource) validatePaymentGuard(index int, p *GuardPayment) error {
	if p.ForEach == "" || p.Reference == "" || p.Amount == "" || p.Currency == "" {
		return fmt.Errorf("command %q: require %d payment needs for_each, reference, amount and currency", c.FullName(), index)
	}
	if !strings.HasPrefix(p.ForEach, "$input.") {
		return fmt.Errorf("command %q: require %d payment for_each must reference $input", c.FullName(), index)
	}
	for _, ref := range []string{p.Reference, p.Amount} {
		if !strings.HasPrefix(ref, "$item.") {
			return fmt.Errorf("command %q: require %d payment reference and amount must use $item", c.FullName(), index)
		}
	}
	if strings.HasPrefix(p.Currency, "$item.") || strings.HasPrefix(p.Currency, "$steps.") {
		return fmt.Errorf("command %q: require %d payment currency cannot use %q", c.FullName(), index, p.Currency)
	}
	return nil
}

func (k *Kernel) consumePaymentGuard(dbTx *sql.Tx, site, opID string, p *GuardPayment, env refEnv, index int) *contract.Error {
	invalid := func(format string, args ...any) *contract.Error {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("require %d: "+format, append([]any{index}, args...)...))
	}
	list, err := resolveEnvRef(p.ForEach, env)
	if err != nil {
		return invalid("%v", err)
	}
	items, ok := list.([]any)
	if !ok {
		return invalid("payment for_each %q must resolve to a list", p.ForEach)
	}
	currency, err := resolveEnvStringRef(p.Currency, env)
	if err != nil {
		return invalid("%v", err)
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return invalid("payment elements must be objects")
		}
		env.item = item
		ref, err := resolveEnvStringRef(p.Reference, env)
		if err != nil || ref == "" {
			return invalid("payment reference is required")
		}
		amountRef, err := resolveEnvRef(p.Amount, env)
		if err != nil {
			return invalid("%v", err)
		}
		amount, err := decimal.NewFromString(fmt.Sprint(amountRef))
		if err != nil {
			return invalid("payment %q amount is not a number", ref)
		}
		if cerr := consumePaymentIntent(dbTx, k.dialect(), site, opID, ref, amount, currency); cerr != nil {
			return cerr
		}
	}
	return nil
}

// consumePaymentIntent locks the intent row, checks it, and marks it consumed.
// The row lock (or the conditional UPDATE where FOR UPDATE is unavailable)
// makes concurrent consumers of one payment mutually exclusive.
func consumePaymentIntent(dbTx *sql.Tx, dialect db.Dialect, site, opID, ref string, amount decimal.Decimal, currency string) *contract.Error {
	reject := func(why string) *contract.Error {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("payment %q %s", ref, why))
	}
	query := "SELECT state, amount, currency, consumed_by FROM _kora_payment_intent WHERE id = ? AND site = ?"
	if named, ok := dialect.(interface{ DriverName() string }); !ok || named.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	var state, storedAmount, storedCurrency string
	var consumed sql.NullString
	if err := dbTx.QueryRow(db.Rebind(dialect, query), ref, site).Scan(&state, &storedAmount, &storedCurrency, &consumed); err != nil {
		if err == sql.ErrNoRows {
			return reject("not found")
		}
		return contract.NewError(contract.CodeDependencyUnavailable, fmt.Sprintf("payment %q check failed", ref))
	}
	stored, err := decimal.NewFromString(storedAmount)
	switch {
	case state != "confirmed":
		return reject("is not confirmed")
	case err != nil || !stored.Equal(amount):
		return reject("amount does not match")
	case storedCurrency != currency:
		return reject("currency does not match")
	case consumed.Valid && consumed.String != "":
		return reject("was already used")
	}
	res, err := dbTx.Exec(db.Rebind(dialect, "UPDATE _kora_payment_intent SET consumed_by = ? WHERE id = ? AND site = ? AND (consumed_by IS NULL OR consumed_by = '')"), opID, ref, site)
	if err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, fmt.Sprintf("payment %q consume failed", ref))
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return reject("was already used")
	}
	return nil
}
