package doctype

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The pos-base pack keeps books in one base currency and lets every money
// record carry its own currency. These tests pin that convention so a later
// change cannot quietly drop it, and they catch the dangling-link mistake that
// once stopped a workspace from being set up.

const posBasePath = "../config/pos-base"

func loadPOSBase(t *testing.T) map[string]*DocType {
	t.Helper()
	doctypes, err := ParseConfigTree(posBasePath)
	if err != nil {
		t.Fatalf("parse pos-base: %v", err)
	}
	byName := make(map[string]*DocType, len(doctypes))
	for _, dt := range doctypes {
		byName[dt.Name] = dt
	}
	return byName
}

func TestPOSBaseHasNoDanglingLinks(t *testing.T) {
	doctypes, err := ParseConfigTree(posBasePath)
	if err != nil {
		t.Fatalf("parse pos-base: %v", err)
	}
	for _, problem := range ValidateAll(doctypes) {
		t.Errorf("%s", problem.Error())
	}
}

// moneyRecord lists each record that holds an amount in a chosen currency and
// the amount field its base-currency twin is computed from.
var posBaseMoneyRecords = map[string]string{
	"Sale":          "total",
	"Payment":       "amount",
	"Cash Movement": "amount",
	"Expense":       "amount",
}

func TestPOSBaseMoneyRecordsCarryCurrencyAndFrozenRate(t *testing.T) {
	dts := loadPOSBase(t)
	for name, amountField := range posBaseMoneyRecords {
		dt := dts[name]
		if dt == nil {
			t.Errorf("%s is missing from pos-base", name)
			continue
		}
		currency := dt.GetField("currency")
		if currency == nil || currency.Fieldtype != "Link" || currency.Options != "Currency" {
			t.Errorf("%s needs a currency field linked to Currency", name)
		}
		rate := dt.GetField("exchange_rate")
		if rate == nil || rate.Fieldtype != "Float" || rate.Default != "1" {
			t.Errorf("%s needs a Float exchange_rate that defaults to 1", name)
		} else if !hasMinConstraintAboveZero(rate) {
			t.Errorf("%s exchange_rate must be constrained to more than zero", name)
		}
		amount := dt.GetField(amountField)
		if amount == nil || amount.Fieldtype != "Currency" {
			t.Errorf("%s needs a Currency field %q", name, amountField)
		}
		base := dt.GetField("base_" + amountField)
		// round() reads text as a number, so the Engine's text default and typed text rates both work.
		want := fmt.Sprintf("(round (* (round %s 9) (round exchange_rate 9)) 2)", amountField)
		if base == nil || base.Fieldtype != "Currency" || base.Computed != want || !base.ReadOnly {
			t.Errorf("%s needs a read-only Currency field base_%s computed as %q", name, amountField, want)
		}
	}
}

func hasMinConstraintAboveZero(f *Field) bool {
	for _, c := range f.Constraints {
		if c.Type != "min" {
			continue
		}
		switch v := c.Value.(type) {
		case float64:
			return v > 0
		case int:
			return v > 0
		}
	}
	return false
}

func TestPOSBaseCurrencyIsOnlyChangedByManagers(t *testing.T) {
	_, permissions, err := ParseRolesDirectory(filepath.Join(posBasePath, "doctypes"))
	if err != nil {
		t.Fatalf("parse permissions: %v", err)
	}
	managerWrites := false
	for _, p := range permissions {
		if p.Doctype != "Currency" {
			continue
		}
		if p.Role == "Cashier" && (p.Write || p.Create || p.Delete) {
			t.Errorf("Cashier must not change currencies or rates")
		}
		if p.Role == "Manager" && p.Write && p.Create {
			managerWrites = true
		}
	}
	if !managerWrites {
		t.Errorf("Manager must be able to add currencies and update rates")
	}
}

// Nothing in the pack may name a currency in code or views. Currencies are data
// the owner enters, and the base currency is whichever record is ticked.
func TestPOSBaseViewsAndDoctypesNameNoCurrency(t *testing.T) {
	banned := []string{"KES", "USD", "EUR", "UGX", "TZS", "KSh"}
	for _, dir := range []string{"doctypes", "views", "reports"} {
		entries, err := os.ReadDir(filepath.Join(posBasePath, dir))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			body, err := os.ReadFile(filepath.Join(posBasePath, dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, code := range banned {
				// Descriptions may give an example, so only look at keys that
				// carry behaviour: defaults and values.
				for _, line := range strings.Split(string(body), "\n") {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "description:") || strings.HasPrefix(trimmed, "message:") || strings.HasPrefix(trimmed, "#") {
						continue
					}
					if strings.Contains(trimmed, code) {
						t.Errorf("%s/%s hard-codes %q: %s", dir, entry.Name(), code, trimmed)
					}
				}
			}
		}
	}
}

func TestPOSBaseDemoFixtureHasExactlyOneBaseCurrency(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(posBasePath, "fixtures", "demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture demoFixture
	if err := yaml.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	bases := 0
	for _, record := range fixture.Records {
		if record["doctype"] != "Currency" {
			continue
		}
		rate, _ := record["rate_to_base"].(int)
		rateFloat, _ := record["rate_to_base"].(float64)
		if record["is_base"] == true {
			bases++
			if rate != 1 && rateFloat != 1 {
				t.Errorf("the base currency must have rate 1")
			}
		}
	}
	if bases != 1 {
		t.Errorf("demo data must contain exactly one base currency, found %d", bases)
	}
}

// MySQL rejects the table SQL the Engine writes for a Date field whose default
// is "today" (it emits DEFAULT CURRENT_DATE without parentheses), so a pack
// that uses it cannot be set up at all. Fill dates when the record is made.
func TestPOSBaseAvoidsDateDefaultsThatBreakMySQL(t *testing.T) {
	for name, dt := range loadPOSBase(t) {
		for _, f := range dt.Fields {
			if f.Fieldtype == "Date" && strings.EqualFold(strings.TrimSpace(f.Default), "today") {
				t.Errorf("%s.%s: a Date default of \"today\" breaks table creation on MySQL", name, f.Fieldname)
			}
		}
	}
}

// The base total must be worked out whether the rate arrives as a number (from
// an API call) or as the text default the Engine fills in when none is sent.
func TestPOSBaseBaseTotalIsComputedFromDefaultAndTypedRates(t *testing.T) {
	sale := loadPOSBase(t)["Sale"]
	cases := []struct {
		name string
		rate any
		want float64
	}{
		{"default rate (text 1)", nil, 300},
		{"typed rate as number", 129.5, 38850},
		{"typed rate as text", "129.5", 38850},
		{"small rate", 0.0345, 10.35},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := NewDocument("")
			doc.Set("total", 300.0)
			if tc.rate != nil {
				doc.Set("exchange_rate", tc.rate)
			}
			ApplyDefaultValues(sale, doc)
			if err := ComputeFields(sale, doc); err != nil {
				t.Fatal(err)
			}
			got, ok := toFloat64(doc.Get("base_total"))
			if !ok {
				t.Fatalf("base_total = %#v (%T), want a number", doc.Get("base_total"), doc.Get("base_total"))
			}
			if diff := got - tc.want; diff > 0.005 || diff < -0.005 {
				t.Fatalf("base_total = %v, want %v", got, tc.want)
			}
		})
	}
}
