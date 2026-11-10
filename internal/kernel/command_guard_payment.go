package kernel

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
)

// GuardPayment requires every payment intent in a list to be confirmed, to
// match the stated amount and currency, and to be unused. The intent is then
// marked consumed by the executing operation, in the command transaction.
var paymentRefField = regexp.MustCompile(`^\$(input|item)\.[A-Za-z_][A-Za-z0-9_]*$`)

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
	if c.Input.Record == "" {
		return fmt.Errorf("command %q: require %d payment needs input.record as the payment source", c.FullName(), index)
	}
	if !strings.HasPrefix(p.ForEach, "$input.") {
		return fmt.Errorf("command %q: require %d payment for_each must reference $input", c.FullName(), index)
	}
	for _, ref := range []string{p.Reference, p.Amount} {
		if !strings.HasPrefix(ref, "$item.") {
			return fmt.Errorf("command %q: require %d payment reference and amount must use $item", c.FullName(), index)
		}
	}
	if !strings.HasPrefix(p.Currency, "$input.") {
		return fmt.Errorf("command %q: require %d payment currency must reference $input", c.FullName(), index)
	}
	for _, ref := range []string{p.ForEach, p.Reference, p.Amount, p.Currency} {
		if !paymentRefField.MatchString(ref) {
			return fmt.Errorf("command %q: require %d payment reference %q must be a plain field", c.FullName(), index, ref)
		}
	}
	return nil
}

// paymentClaim names one payment intent an operation consumes. Site, OpID and
// Source are fixed per guard; Ref, Amount and Currency are filled per intent.
type paymentClaim struct {
	Site, OpID, Source string
	Ref                string
	Amount             decimal.Decimal
	Currency           string
}

func (k *Kernel) consumePaymentGuard(dbTx *sql.Tx, claim paymentClaim, p *GuardPayment, env refEnv, index int) *contract.Error {
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
	claim.Currency = currency
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
		claim.Ref, claim.Amount = ref, amount
		if cerr := consumePaymentIntent(dbTx, k.dialect(), claim); cerr != nil {
			return cerr
		}
	}
	return nil
}

// consumePaymentIntent locks the intent row, checks it, and marks it consumed.
// The row lock (or the conditional UPDATE where FOR UPDATE is unavailable)
// makes concurrent consumers of one payment mutually exclusive.
func consumePaymentIntent(dbTx *sql.Tx, dialect db.Dialect, claim paymentClaim) *contract.Error {
	site, opID, ref, source, amount, currency := claim.Site, claim.OpID, claim.Ref, claim.Source, claim.Amount, claim.Currency
	reject := func(why string) *contract.Error {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("payment %q %s", ref, why))
	}
	query := "SELECT state, amount, currency, source_doctype, consumed_by FROM _kora_payment_intent WHERE id = ? AND site = ?"
	if named, ok := dialect.(interface{ DriverName() string }); !ok || named.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	var state, storedAmount, storedCurrency, storedSource string
	var consumed sql.NullString
	if err := dbTx.QueryRow(db.Rebind(dialect, query), ref, site).Scan(&state, &storedAmount, &storedCurrency, &storedSource, &consumed); err != nil {
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
	case storedSource != source:
		return reject("source does not match")
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
