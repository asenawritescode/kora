package engineadapter

import (
	"database/sql"
	"testing"
	"time"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/engine"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/scheduler"
)

func TestNextSubscriptionBillingDateAnchorsMonthEnd(t *testing.T) {
	start := time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC)
	february, err := nextSubscriptionBillingDate(start, start, "Monthly", 1)
	if err != nil || february.Format("2006-01-02") != "2026-02-28" {
		t.Fatalf("February billing date = %s, %v", february.Format("2006-01-02"), err)
	}
	march, err := nextSubscriptionBillingDate(february, start, "Monthly", 1)
	if err != nil || march.Format("2006-01-02") != "2026-03-31" {
		t.Fatalf("March billing date = %s, %v; want anchored month end", march.Format("2006-01-02"), err)
	}
}

func TestRecurringInvoiceNumberIsStableForBillingPeriod(t *testing.T) {
	t.Parallel()

	billingDate := time.Date(2026, time.October, 20, 0, 0, 0, 0, time.UTC)
	want := "SUBS-0001-20261020"
	if got := recurringInvoiceNumber("SUBS-0001", billingDate); got != want {
		t.Fatalf("recurring invoice number = %q, want %q", got, want)
	}
}

func TestScheduledJobHandlersAreOptInPerTenantPackage(t *testing.T) {
	registry := doctype.NewRegistry()
	runtime := engine.TenantJobRuntime{Database: &sql.DB{}, Registry: registry, Kernel: &kernel.Kernel{}}
	if got := ScheduledJobHandlers(runtime); len(got) != 0 {
		t.Fatalf("handlers without subscription doctypes = %#v, want none", got)
	}
	for _, name := range []string{"Subscription", "Plan", "Subscription Line", "Billing Schedule", "Recurring Invoice"} {
		registry.Register(&doctype.DocType{Name: name})
	}
	got := ScheduledJobHandlers(runtime)
	if got[JobType] == nil || len(got) != 1 {
		t.Fatalf("handlers with subscription doctypes = %#v, want the application-owned invoice handler", got)
	}
	if scheduler.JobType("subscription_invoice") != JobType {
		t.Fatal("invoice handler type does not match its configured job type")
	}
}

func TestConfiguredInvoiceDueDays(t *testing.T) {
	for _, test := range []struct {
		config map[string]any
		want   int
		bad    bool
	}{
		{config: nil, want: 0},
		{config: map[string]any{"due_days": float64(14)}, want: 14},
		{config: map[string]any{"due_days": "7"}, want: 7},
		{config: map[string]any{"due_days": float64(1.5)}, bad: true},
		{config: map[string]any{"due_days": float64(366)}, bad: true},
	} {
		got, err := configuredDueDays(test.config)
		if test.bad && err == nil {
			t.Fatalf("configuredDueDays(%v) error = nil, want validation error", test.config)
		}
		if !test.bad && (err != nil || got != test.want) {
			t.Fatalf("configuredDueDays(%v) = %d, %v; want %d", test.config, got, err, test.want)
		}
	}
}
