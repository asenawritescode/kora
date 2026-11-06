// Package manual records a payment received outside the system, such as on a
// till or paybill number, without any provider integration. The cashier
// supplies the receiving account and the payer's confirmation code; nothing
// confirms the payment with a provider, so the intent is labelled unverified.
package manual

import (
	"context"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/payment"
)

const (
	// Name is the provider name used in payment requests.
	Name = "manual"
	// Recorded labels a payment nobody confirmed with a provider.
	Recorded = "manually_recorded"
	// Confirmed labels a payment a provider confirmed.
	Confirmed = "provider_confirmed"

	maxCodeLen = 100
	// requestIDLen is the width of the intent's provider request id column.
	requestIDLen = 190
)

// Account is what the provider needs to know about a receiving account.
type Account struct {
	Enabled  bool
	Currency string
}

// AccountLookup finds a receiving account by name.
type AccountLookup interface {
	Account(ctx context.Context, name string) (Account, bool, error)
}

// Provider confirms a manual payment immediately. It implements
// payment.Provider.
type Provider struct{ Accounts AccountLookup }

// Name returns "manual".
func (Provider) Name() string { return Name }

// Request validates the account and confirmation code and confirms the intent.
// The account and normalised code become the provider request id, so the
// store's unique (site, provider, request id) index makes a code usable once
// per account; the code alone is the provider reference.
func (p Provider) Request(ctx context.Context, in payment.Intent) (payment.Outcome, error) {
	invalid := func(msg string) (payment.Outcome, error) {
		return payment.Outcome{}, contract.NewError(contract.CodeValidationFailed, msg)
	}
	account := strings.TrimSpace(in.Account)
	code := strings.ToUpper(strings.TrimSpace(in.Reference))
	switch {
	case account == "":
		return invalid("payment account is required")
	case code == "":
		return invalid("confirmation code is required")
	case len(code) > maxCodeLen:
		return invalid("confirmation code is too long")
	case len(Key(account, code)) > requestIDLen:
		return invalid("payment account name is too long")
	}
	acct, found, err := p.Accounts.Account(ctx, account)
	switch {
	case err != nil:
		return payment.Outcome{}, fmt.Errorf("manual: look up payment account: %w", err)
	case !found:
		return invalid("payment account not found")
	case !acct.Enabled:
		return invalid("payment account is disabled")
	case acct.Currency != "" && !strings.EqualFold(acct.Currency, in.Currency):
		return invalid("payment currency does not match the account currency")
	}
	return payment.Outcome{State: payment.StateConfirmed, ProviderRequestID: Key(account, code), ProviderReference: code}, nil
}

// Key is the provider request id of a manual payment.
func Key(account, code string) string { return account + ":" + code }

// Split recovers the account and code from a manual payment's provider request
// id; ok is false when the id does not carry an account.
func Split(requestID string) (account, code string, ok bool) {
	account, code, ok = strings.Cut(requestID, ":")
	return account, code, ok && account != "" && code != ""
}

// Verification labels how a payment from provider was confirmed. Cash is not
// confirmed by any provider and has no label.
func Verification(provider string) string {
	switch provider {
	case Name:
		return Recorded
	case "cash", "":
		return ""
	default:
		return Confirmed
	}
}
