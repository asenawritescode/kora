//go:build integration

package api

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// The parked accounting pack (on top of the selling pack's Currency) loaded as shipped, with its
// scripts run the way the Engine runs them. The Accountant keeps the books; the Administrator
// stands in for the delete rules, since the Accountant has no delete permission.
const erpAccountingPack = "../../config/_parked/erp-accounting"

func TestPackScriptsAccounting(t *testing.T) {
	eachPackDialect(t, []string{erpSellingPack, erpAccountingPack}, func(f *packFixture) {
		for _, currency := range []string{"KES", "USD"} {
			f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, ?)", currency, currency, currency, true)
		}
		accountingScenario(f)
	})
}

func accountingScenario(f *packFixture) {
	var (
		assets, cash, sales, retail, rent, old, suspense      string
		draft, posted, reversedOriginal, periodOct, periodNov string
		periodCurrent, filled, postedUnderClosed              string
	)
	const (
		oct = "2019-10-15"
		nov = "2019-11-15"
		dec = "2019-12-15"
	)

	f.run("chart keeps child types matching their parent", func(f *packFixture) {
		books := f.as("Accountant")
		assets = accountingCreate(books, "Account", map[string]any{"code": "1000", "account_name": "Assets", "account_type": "Asset", "allow_posting": false})
		cash = accountingCreate(books, "Account", map[string]any{"code": "1010", "account_name": "Cash", "account_type": "Asset", "parent_account": assets})
		sales = accountingCreate(books, "Account", map[string]any{"code": "4000", "account_name": "Sales", "account_type": "Income", "allow_posting": false})
		retail = accountingCreate(books, "Account", map[string]any{"code": "4100", "account_name": "Retail Sales", "account_type": "Income", "parent_account": sales})
		rent = accountingCreate(books, "Account", map[string]any{"code": "5000", "account_name": "Rent", "account_type": "Expense"})
		old = accountingCreate(books, "Account", map[string]any{"code": "6000", "account_name": "Old Fees", "account_type": "Expense", "status": "Inactive"})
		suspense = accountingCreate(books, "Account", map[string]any{"code": "7000", "account_name": "Suspense", "account_type": "Expense"})

		books.refused(httpMethodPost, "Account", "", jsonBody(f.t, map[string]any{"code": "1020", "account_name": "Loan", "account_type": "Expense", "parent_account": assets}), "a child account must have the same type")
		books.refused(httpMethodPut, "Account", assets, `{"account_type":"Liability"}`, "same type")
	})

	f.run("a draft is free to change until it posts", func(f *packFixture) {
		books := f.as("Accountant")
		draft = books.create("Journal Entry", accountingEntryBody(f, "Draft", oct,
			accountingLine(cash, 100, 0), accountingLine(retail, 0, 100)))
		books.update("Journal Entry", draft, `{"memo":"edited draft"}`)
		books.expect("Journal Entry", draft, "memo", "edited draft")
	})

	f.run("lines must be postable and balanced", func(f *packFixture) {
		books := f.as("Accountant")
		entry := func(lines ...map[string]any) string {
			return accountingEntryBody(f, "Draft", oct, lines...)
		}
		books.refused(httpMethodPost, "Journal Entry", "", entry(accountingLine(sales, 100, 0), accountingLine(cash, 0, 100)), "4000 Sales is a group account and takes no postings.")
		books.refused(httpMethodPost, "Journal Entry", "", entry(accountingLine(old, 100, 0), accountingLine(cash, 0, 100)), "6000 Old Fees is inactive.")
		books.refused(httpMethodPost, "Journal Entry", "", entry(accountingLine(cash, 100, 100), accountingLine(retail, 0, 0)), "Line 1 cannot have a debit and a credit.")
		books.refused(httpMethodPost, "Journal Entry", "", entry(accountingLine(cash, 100, 0), accountingLine(retail, 0, 0), accountingLine(rent, 0, 100)), "Line 2 needs a debit or a credit.")
		books.refused(httpMethodPost, "Journal Entry", "", entry(accountingLine(cash, -100, 0), accountingLine(retail, 0, -100)), "Line 1 cannot be negative.")
		books.refused(httpMethodPost, "Journal Entry", "", entry(accountingLine(cash, 100, 0), accountingLine(retail, 0, 90)), "Debits and credits must be equal.")
		books.refused(httpMethodPost, "Journal Entry", "", entry(accountingLine(cash, 0, 0)), "needs at least")
	})

	f.run("posting checks status moves and freezes the entry", func(f *packFixture) {
		books := f.as("Accountant")
		books.refused(httpMethodPost, "Journal Entry", "", accountingEntryBody(f, "Reversed", oct,
			accountingLine(cash, 100, 0), accountingLine(retail, 0, 100)), "only after it is posted")
		books.refused(httpMethodPut, "Journal Entry", draft, `{"status":"Reversed"}`, "only after it is posted")

		books.update("Journal Entry", draft, `{"status":"Posted"}`)
		books.expect("Journal Entry", draft, "status", "Posted")
		posted = draft

		books.refused(httpMethodPut, "Journal Entry", posted, `{"status":"Draft"}`, "cannot go back to Draft")
		books.refused(httpMethodPut, "Journal Entry", posted, `{"posting_date":"2019-10-16"}`, "cannot change")
		books.refused(httpMethodPut, "Journal Entry", posted, `{"currency":"USD"}`, "cannot change")
		books.refused(httpMethodPut, "Journal Entry", posted, `{"lines":[{"account":"`+cash+`","debit":200,"credit":0},{"account":"`+retail+`","debit":0,"credit":200}]}`, "cannot change")
		books.update("Journal Entry", posted, `{"memo":"audited"}`)
		books.expect("Journal Entry", posted, "memo", "audited")

		// No Accounting Period exists yet, so these postings pass without a period check.
		created := books.create("Journal Entry", accountingEntryBody(f, "Posted", oct,
			accountingLine(cash, 60, 0), accountingLine(retail, 0, 60)))
		books.expect("Journal Entry", created, "status", "Posted")
	})

	f.run("only drafts can be deleted", func(f *packFixture) {
		f.refused(httpMethodDelete, "Journal Entry", posted, "", "cannot be deleted")
		doomed := f.create("Journal Entry", accountingEntryBody(f, "Draft", oct,
			accountingLine(cash, 10, 0), accountingLine(retail, 0, 10)))
		if code, _, _ := f.send(httpMethodDelete, "Journal Entry", doomed, ""); code != 200 {
			f.t.Fatalf("deleting a draft = %d, want 200", code)
		}
	})

	f.run("accounting periods do not overlap and a blank period is filled", func(f *packFixture) {
		books := f.as("Accountant")
		periodOct = accountingCreate(books, "Accounting Period", map[string]any{"period_name": "2019-10", "start_date": "2019-10-01", "end_date": "2019-10-31", "status": "Open"})
		books.refused(httpMethodPost, "Accounting Period", "", `{"period_name":"2019-10b","start_date":"2019-10-15","end_date":"2019-11-10","status":"Open"}`, "overlaps 2019-10")
		periodNov = accountingCreate(books, "Accounting Period", map[string]any{"period_name": "2019-11", "start_date": "2019-11-01", "end_date": "2019-11-30", "status": "Open"})

		filled = books.create("Journal Entry", accountingEntryBody(f, "Draft", oct,
			accountingLine(cash, 70, 0), accountingLine(retail, 0, 70)))
		books.expect("Journal Entry", filled, "period", periodOct)

	})

	f.run("a period must cover the date and be open to post", func(f *packFixture) {
		books := f.as("Accountant")
		body := func(period string) string {
			return jsonBody(f.t, map[string]any{"posting_date": oct, "status": "Draft", "period": period, "lines": []map[string]any{
				accountingLine(cash, 80, 0), accountingLine(retail, 0, 80),
			}})
		}
		books.refused(httpMethodPost, "Journal Entry", "", body(periodNov), "2019-11 does not cover "+oct)

		books.update("Accounting Period", periodOct, `{"status":"Closed"}`)
		books.refused(httpMethodPut, "Journal Entry", filled, `{"status":"Posted"}`, "2019-10 is Closed.")
		books.update("Accounting Period", periodOct, `{"status":"Open"}`)
		books.update("Journal Entry", filled, `{"status":"Posted"}`)
		books.expect("Journal Entry", filled, "status", "Posted")

		dated := books.create("Journal Entry", accountingEntryBody(f, "Draft", dec,
			accountingLine(cash, 5, 0), accountingLine(retail, 0, 5)))
		books.expect("Journal Entry", dated, "status", "Draft")
		books.refused(httpMethodPut, "Journal Entry", dated, `{"status":"Posted"}`, "No accounting period covers "+dec)
	})

	f.run("reversal writes the opposite entry dated today", func(f *packFixture) {
		books := f.as("Accountant")
		now := time.Now().UTC()
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		periodCurrent = accountingCreate(books, "Accounting Period", map[string]any{
			"period_name": now.Format("2006-01"),
			"start_date":  start.Format("2006-01-02"),
			"end_date":    start.AddDate(0, 1, -1).Format("2006-01-02"),
			"status":      "Open",
		})

		reversedOriginal = books.create("Journal Entry", accountingEntryBody(f, "Posted", oct,
			accountingLine(cash, 250, 0), accountingLine(retail, 0, 250)))
		books.expect("Journal Entry", reversedOriginal, "period", periodOct)
		ref := f.value("Journal Entry", reversedOriginal, "reference")

		books.update("Journal Entry", reversedOriginal, `{"status":"Reversed"}`)
		books.expect("Journal Entry", reversedOriginal, "status", "Reversed")

		reversal := accountingReversalOf(f, reversedOriginal)
		if reversal == "" {
			f.t.Fatal("reversing the entry wrote no opposite entry")
		}
		books.expect("Journal Entry", reversal, "status", "Posted")
		if got := f.value("Journal Entry", reversal, "posting_date"); !strings.HasPrefix(got, now.Format("2006-01-02")) {
			f.t.Fatalf("reversal posting date = %q, want today %s", got, now.Format("2006-01-02"))
		}
		books.expect("Journal Entry", reversal, "memo", "Reversal of "+ref)
		books.expect("Journal Entry", reversal, "source_type", "Journal Entry")
		books.expect("Journal Entry", reversal, "source_reference", reversedOriginal)
		books.expect("Journal Entry", reversal, "period", periodCurrent)
		if got := accountingSum(f, "credit", reversal, cash); got != 250 {
			f.t.Fatalf("reversal credits cash %v, want 250", got)
		}
		if got := accountingNet(f, cash, reversedOriginal, reversal); got != 0 {
			f.t.Fatalf("cash across the entry and its reversal nets to %v, want 0", got)
		}
		if got := accountingNet(f, retail, reversedOriginal, reversal); got != 0 {
			f.t.Fatalf("retail sales across the entry and its reversal nets to %v, want 0", got)
		}

		books.refused(httpMethodPut, "Journal Entry", reversedOriginal, `{"status":"Posted"}`, "A reversed entry is final.")
		books.refused(httpMethodPut, "Journal Entry", reversedOriginal, `{"lines":[{"account":"`+cash+`","debit":1,"credit":0},{"account":"`+retail+`","debit":0,"credit":1}]}`, "cannot change")
		books.update("Journal Entry", reversedOriginal, `{"memo":"reversed in error note"}`)
	})

	f.run("reversal is refused while today's period is closed", func(f *packFixture) {
		books := f.as("Accountant")
		postedUnderClosed = books.create("Journal Entry", accountingEntryBody(f, "Posted", oct,
			accountingLine(cash, 40, 0), accountingLine(retail, 0, 40)))
		books.update("Accounting Period", periodCurrent, `{"status":"Closed"}`)
		books.refused(httpMethodPut, "Journal Entry", postedUnderClosed, `{"status":"Reversed"}`, " is Closed.")
		books.expect("Journal Entry", postedUnderClosed, "status", "Posted")
		if got := accountingReversalOf(f, postedUnderClosed); got != "" {
			f.t.Fatalf("a refused reversal left entry %s behind", got)
		}
		books.update("Accounting Period", periodCurrent, `{"status":"Open"}`)
	})

	f.run("accounts used on posted entries keep their type", func(f *packFixture) {
		books := f.as("Accountant")
		books.create("Journal Entry", accountingEntryBody(f, "Posted", oct,
			accountingLine(rent, 30, 0), accountingLine(cash, 0, 30)))
		books.refused(httpMethodPut, "Account", rent, `{"account_type":"Asset"}`, "its type cannot change")
		books.update("Account", suspense, `{"account_type":"Income"}`)
		books.expect("Account", suspense, "account_type", "Income")
	})

	f.run("an account used on a posted entry cannot be deleted", func(f *packFixture) {
		f.refused(httpMethodDelete, "Account", rent, "", "cannot be deleted")
		if code, _, _ := f.send(httpMethodDelete, "Account", suspense, ""); code != 200 {
			f.t.Fatalf("deleting an unused account = %d, want 200", code)
		}
	})

}

// accountingCreate saves a new record of the given fields and returns its name.
func accountingCreate(f *packFixture, recordType string, fields map[string]any) string {
	f.t.Helper()
	return f.create(recordType, jsonBody(f.t, fields))
}

// accountingLine is one journal line for a request body.
func accountingLine(account string, debit, credit float64) map[string]any {
	return map[string]any{"account": account, "debit": debit, "credit": credit}
}

// accountingEntryBody is a journal entry request body with a memo and the given lines.
func accountingEntryBody(f *packFixture, status, date string, lines ...map[string]any) string {
	f.t.Helper()
	return jsonBody(f.t, map[string]any{"posting_date": date, "status": status, "memo": "test entry", "lines": lines})
}

// accountingReversalOf returns the entry written as the reversal of original, or "".
func accountingReversalOf(f *packFixture, original string) string {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT name FROM " + dl.QuoteIdent("tabJournal Entry") + " WHERE source_reference = ?"
	var name sql.NullString
	err := f.db.QueryRow(kdb.Rebind(dl, query), original).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		f.t.Fatalf("find reversal of %s: %v", original, err)
	}
	return name.String
}

// accountingSum adds one line column (debit or credit) of an account within one entry.
func accountingSum(f *packFixture, column, entry, account string) float64 {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT COALESCE(SUM(" + dl.QuoteIdent(column) + "), 0) FROM " + dl.QuoteIdent("tabJournal Entry__lines") + " WHERE parent = ? AND account = ?"
	var sum float64
	if err := f.db.QueryRow(kdb.Rebind(dl, query), entry, account).Scan(&sum); err != nil {
		f.t.Fatalf("sum %s of %s on %s: %v", column, account, entry, err)
	}
	return sum
}

// accountingNet is the debit minus credit an account holds across the given entries.
func accountingNet(f *packFixture, account string, entries ...string) float64 {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(entries)), ", ")
	query := "SELECT COALESCE(SUM(debit), 0) - COALESCE(SUM(credit), 0) FROM " + dl.QuoteIdent("tabJournal Entry__lines") +
		" WHERE account = ? AND parent IN (" + marks + ")"
	args := []any{account}
	for _, entry := range entries {
		args = append(args, entry)
	}
	var net float64
	if err := f.db.QueryRow(kdb.Rebind(dl, query), args...).Scan(&net); err != nil {
		f.t.Fatalf("net of %s: %v", account, err)
	}
	return net
}
