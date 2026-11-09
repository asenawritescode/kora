package manual

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/payment"
)

type fakeAccounts struct {
	accounts map[string]Account
	err      error
}

func (f fakeAccounts) Account(_ context.Context, name string) (Account, bool, error) {
	a, ok := f.accounts[name]
	return a, ok, f.err
}

func TestProviderRequest(t *testing.T) {
	accounts := fakeAccounts{accounts: map[string]Account{
		"ACC-1": {Enabled: true, Currency: "KES"},
		"ACC-2": {Enabled: false, Currency: "KES"},
		"ACC-3": {Enabled: true, Currency: "USD"},
	}}
	tests := []struct {
		name      string
		accounts  AccountLookup
		in        payment.Intent
		wantErr   string // substring of a validation error
		wantInfra bool
		wantReqID string
		wantRef   string
	}{
		{"valid, code normalised", accounts, payment.Intent{Account: "ACC-1", Reference: " qk12ab34cd ", Currency: "KES"}, "", false, "ACC-1:QK12AB34CD", "QK12AB34CD"},
		{"missing code", accounts, payment.Intent{Account: "ACC-1", Reference: "  ", Currency: "KES"}, "confirmation code is required", false, "", ""},
		{"missing account", accounts, payment.Intent{Reference: "QK1", Currency: "KES"}, "payment account is required", false, "", ""},
		{"unknown account", accounts, payment.Intent{Account: "ACC-9", Reference: "QK1", Currency: "KES"}, "payment account not found", false, "", ""},
		{"disabled account", accounts, payment.Intent{Account: "ACC-2", Reference: "QK1", Currency: "KES"}, "payment account is disabled", false, "", ""},
		{"wrong currency", accounts, payment.Intent{Account: "ACC-3", Reference: "QK1", Currency: "KES"}, "currency does not match", false, "", ""},
		{"code too long", accounts, payment.Intent{Account: "ACC-1", Reference: strings.Repeat("A", 101), Currency: "KES"}, "confirmation code is too long", false, "", ""},
		{"lookup failure is not a validation error", fakeAccounts{err: errors.New("db down")}, payment.Intent{Account: "ACC-1", Reference: "QK1", Currency: "KES"}, "", true, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Provider{Accounts: tc.accounts}.Request(context.Background(), tc.in)
			switch {
			case tc.wantErr != "":
				var ce *contract.Error
				if !errors.As(err, &ce) || ce.Type != contract.CodeValidationFailed || !strings.Contains(ce.Message, tc.wantErr) {
					t.Fatalf("err = %v, want validation error containing %q", err, tc.wantErr)
				}
			case tc.wantInfra:
				var ce *contract.Error
				if err == nil || errors.As(err, &ce) {
					t.Fatalf("err = %v, want a plain infrastructure error", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if out.State != payment.StateConfirmed || out.ProviderRequestID != tc.wantReqID || out.ProviderReference != tc.wantRef {
					t.Fatalf("outcome = %+v", out)
				}
			}
		})
	}
}

func TestKeySplit(t *testing.T) {
	for _, tc := range []struct{ account, code string }{{"ACC-1", "QK1"}, {"A", "B:C"}} {
		account, code, ok := Split(Key(tc.account, tc.code))
		if !ok || account != tc.account || code != tc.code {
			t.Errorf("Split(Key(%q,%q)) = %q,%q,%v", tc.account, tc.code, account, code, ok)
		}
	}
	if _, _, ok := Split("req-123"); ok {
		t.Error("a provider request id without an account must not split")
	}
}

func TestVerification(t *testing.T) {
	for provider, want := range map[string]string{"manual": "manually_recorded", "mpesa": "provider_confirmed", "cash": ""} {
		if got := Verification(provider); got != want {
			t.Errorf("Verification(%q) = %q, want %q", provider, got, want)
		}
	}
}
