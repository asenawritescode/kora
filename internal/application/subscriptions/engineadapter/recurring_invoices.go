package engineadapter

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/engine"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/scheduler"
)

const recurringInvoiceCatchUpLimit = 24

const JobType scheduler.JobType = "subscription_invoice"

type recurringInvoiceGenerator struct {
	database       *sql.DB
	registry       *doctype.Registry
	dialect        kdb.Dialect
	kernel         *kernel.Kernel
	tenantID       string
	siteName       string
	configRevision uint64
	billing        BillingProvider
}

type BillingObligation struct {
	Source         contract.BusinessRef
	PeriodStart    string
	PeriodEnd      string
	Customer       contract.BusinessRef
	Currency       string
	Lines          []BillingLine
	IdempotencyKey string
}

type BillingLine struct {
	Description string
	Quantity    string
	UnitPrice   string
}
type BillingResult struct {
	Obligation        contract.BusinessRef
	State             string
	ExternalReference string
}
type BillingProvider interface {
	CreateObligation(context.Context, contract.ScopeContext, BillingObligation) (BillingResult, error)
	ReconcileObligation(context.Context, contract.ScopeContext, string) (BillingResult, error)
}

func (o BillingObligation) Validate() error {
	if o.Source.ID == "" || o.Source.ProviderID == "" || o.Source.ResourceType == "" || o.Customer.ID == "" || o.Customer.ProviderID == "" || o.Currency == "" || o.IdempotencyKey == "" || len(o.Lines) == 0 {
		return fmt.Errorf("billing obligation identity, customer, currency, lines and idempotency key are required")
	}
	start, err := time.Parse("2006-01-02", o.PeriodStart)
	if err != nil {
		return fmt.Errorf("invalid billing period start")
	}
	end, err := time.Parse("2006-01-02", o.PeriodEnd)
	if err != nil || end.Before(start) {
		return fmt.Errorf("invalid billing period end")
	}
	if len(o.Lines) > 100 {
		return fmt.Errorf("billing obligation has too many lines")
	}
	for _, line := range o.Lines {
		quantity, err := strconv.ParseFloat(line.Quantity, 64)
		if err != nil || math.IsNaN(quantity) || math.IsInf(quantity, 0) || quantity <= 0 {
			return fmt.Errorf("billing quantity must be a positive decimal")
		}
		price, err := strconv.ParseFloat(line.UnitPrice, 64)
		if err != nil || math.IsNaN(price) || math.IsInf(price, 0) || price < 0 {
			return fmt.Errorf("billing unit price must be a nonnegative decimal")
		}
		if line.Description == "" || len(line.Description) > 500 {
			return fmt.Errorf("billing line description is invalid")
		}
	}
	return nil
}

// ScheduledJobHandlers binds subscription invoice automation only when this
// tenant has the subscription pack and the application kernel available.
func ScheduledJobHandlers(runtime engine.TenantJobRuntime) map[scheduler.JobType]scheduler.JobHandler {
	if runtime.Database == nil || runtime.Registry == nil || runtime.Kernel == nil {
		return nil
	}
	for _, name := range []string{"Subscription", "Plan", "Subscription Line"} {
		if runtime.Registry.Get(name) == nil {
			return nil
		}
	}
	generator := &recurringInvoiceGenerator{
		database: runtime.Database, registry: runtime.Registry, dialect: runtime.Dialect,
		kernel: runtime.Kernel, tenantID: runtime.TenantID, siteName: runtime.SiteName,
		configRevision: runtime.ConfigRevision,
	}
	return map[scheduler.JobType]scheduler.JobHandler{
		JobType: func(ctx context.Context, job *scheduler.JobConfig) error {
			var config map[string]any
			if job != nil {
				config = job.Config
			}
			return generator.GenerateDueInvoices(ctx, config)
		},
	}
}

func ScheduledJobHandlersWithBilling(runtime engine.TenantJobRuntime, provider BillingProvider) map[scheduler.JobType]scheduler.JobHandler {
	if provider == nil {
		return nil
	}
	if runtime.Database == nil || runtime.Registry == nil {
		return nil
	}
	for _, name := range []string{"Subscription", "Plan", "Subscription Line"} {
		if runtime.Registry.Get(name) == nil {
			return nil
		}
	}
	generator := &recurringInvoiceGenerator{database: runtime.Database, registry: runtime.Registry, dialect: runtime.Dialect, kernel: runtime.Kernel, tenantID: runtime.TenantID, siteName: runtime.SiteName, configRevision: runtime.ConfigRevision, billing: provider}
	return map[scheduler.JobType]scheduler.JobHandler{JobType: func(ctx context.Context, job *scheduler.JobConfig) error {
		var config map[string]any
		if job != nil {
			config = job.Config
		}
		return generator.GenerateDueInvoices(ctx, config)
	}}
}

func (g *recurringInvoiceGenerator) GenerateDueInvoices(ctx context.Context, config map[string]any) error {
	if ctx == nil || g == nil || g.database == nil || g.registry == nil || g.kernel == nil {
		return errors.New("recurring invoice generator is not fully configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tenantID, err := contract.ParseTenantID(g.tenantID)
	if err != nil {
		return fmt.Errorf("recurring invoice tenant ID: %w", err)
	}
	tx := &orm.TxManager{DB: g.database, Registry: g.registry, Dialect: g.dialect, SiteName: g.siteName}
	actor := contract.ActorContext{
		PrincipalID: "kora:recurring-invoice-scheduler", PrincipalType: contract.PrincipalService,
		Site: g.siteName, AuthenticatedAt: time.Now().UTC(),
	}
	execution, err := policy.NewVerifiedExecutionWithGrants(tenantID, actor, []policy.Grant{
		{Resource: "Subscription", Operation: "read"},
		{Resource: "Plan", Operation: "read"},
		{Resource: "Subscription Line", Operation: "read"},
		{Resource: "Billing Schedule", Operation: "create"},
		{Resource: "Recurring Invoice", Operation: "create"},
		{Resource: "Subscription", Operation: "write"},
	})
	if err != nil {
		return fmt.Errorf("build invoice worker identity: %w", err)
	}
	dueDays, err := configuredDueDays(config)
	if err != nil {
		return err
	}
	dueDateOffset := time.Duration(dueDays) * 24 * time.Hour
	date := time.Now().UTC().Format("2006-01-02")
	filters, _ := json.Marshal([][]any{{"status", "=", "Active"}, {"next_billing_date", "<=", date}})
	subscriptionType := g.registry.Get("Subscription")
	if subscriptionType == nil {
		return errors.New("Subscription doctype is not installed")
	}
	var cursor string
	var generationErrors []error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageFilters := string(filters)
		if cursor != "" {
			var terms [][]any
			if err := json.Unmarshal(filters, &terms); err != nil {
				return err
			}
			terms = append(terms, []any{"name", ">", cursor})
			page, _ := json.Marshal(terms)
			pageFilters = string(page)
		}
		subscriptions, _, err := tx.GetListWithOptions(subscriptionType, pageFilters, "name ASC", 200, 0, "", nil, "")
		if err != nil {
			return fmt.Errorf("list due subscriptions: %w", err)
		}
		if len(subscriptions) == 0 {
			return errors.Join(generationErrors...)
		}
		for _, listed := range subscriptions {
			if listed == nil || listed.Name == "" {
				continue
			}
			cursor = listed.Name
			if err := g.generateForSubscription(ctx, tx, execution, listed.Name, date, dueDateOffset); err != nil {
				// A later run retries this due period. The key is deterministic and
				// the schedule reference is unique, so a committed retry is safe.
				slog.Error("recurring invoice generation failed", "tenant", g.siteName, "subscription", listed.Name, "error", err)
				generationErrors = append(generationErrors, fmt.Errorf("subscription %s: %w", listed.Name, err))
			}
		}
		if len(subscriptions) < 200 {
			return errors.Join(generationErrors...)
		}
	}
}

func (g *recurringInvoiceGenerator) generateForSubscription(ctx context.Context, tx *orm.TxManager, execution policy.Execution, name, today string, dueDateOffset time.Duration) error {
	subscriptionType := g.registry.Get("Subscription")
	subscription, err := tx.GetDoc(subscriptionType, name, "")
	if err != nil {
		return fmt.Errorf("load subscription: %w", err)
	}
	for generated := 0; generated < recurringInvoiceCatchUpLimit; generated++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		billingDate := subscription.GetString("next_billing_date")
		if billingDate == "" || billingDate > today {
			return nil
		}
		start, err := time.Parse("2006-01-02", subscription.GetString("start_date"))
		if err != nil {
			return fmt.Errorf("invalid subscription start date: %w", err)
		}
		due, err := time.Parse("2006-01-02", billingDate)
		if err != nil {
			return fmt.Errorf("invalid next billing date: %w", err)
		}
		if endDate := subscription.GetString("end_date"); endDate != "" && endDate < billingDate {
			return nil // Do not bill beyond the configured subscription term.
		}
		next, err := nextSubscriptionBillingDate(due, start, subscription.GetString("billing_interval"), int(subscription.GetFloat("interval_count")))
		if err != nil {
			return err
		}
		subtotal, taxAmount, amount, err := g.subscriptionTotals(tx, subscription)
		if err != nil {
			return err
		}
		periodEnd := next.AddDate(0, 0, -1).Format("2006-01-02")
		reference := subscription.Name + "-" + due.Format("20060102")
		if g.billing != nil {
			key := recurringInvoiceIdempotencyKey(subscription.Name, billingDate)
			obligation := BillingObligation{Source: contract.BusinessRef{ProviderID: "kora.subscriptions", ResourceType: "subscription", ID: subscription.Name}, PeriodStart: billingDate, PeriodEnd: periodEnd, Customer: contract.BusinessRef{ProviderID: "kora.parties", ResourceType: "party", ID: subscription.GetString("customer")}, Currency: subscription.GetString("currency"), Lines: []BillingLine{{Description: "Subscription " + subscription.Name, Quantity: "1", UnitPrice: strconv.FormatFloat(amount, 'f', -1, 64)}}, IdempotencyKey: key}
			if err := obligation.Validate(); err != nil {
				return err
			}
			scope := contract.ScopeContext{TenantID: contract.TenantID(g.tenantID), ProviderID: "kora.subscriptions"}
			result, err := g.billing.CreateObligation(ctx, scope, obligation)
			if err != nil {
				return fmt.Errorf("create billing obligation %s: %w", reference, err)
			}
			if result.State != "accepted" && result.State != "issued" {
				return fmt.Errorf("billing obligation %s remains %s and requires reconciliation", reference, result.State)
			}
			advance, err := json.Marshal(map[string]any{"next_billing_date": next.Format("2006-01-02")})
			if err != nil {
				return err
			}
			update, err := json.Marshal(kernel.RecordUpdatePayload{Doctype: "Subscription", Name: subscription.Name, Data: advance})
			if err != nil {
				return err
			}
			operation := kernel.Operation{Command: kernel.CommandRecordUpdate, Payload: update, Context: kernel.OperationContext{Site: g.siteName, TenantID: execution.Tenant(), Execution: execution, Source: kernel.SourceWorker, User: "system", IdempotencyKey: key + "-schedule", ConfigRevision: g.configRevision}}
			commandResult, commandErr := g.kernel.Execute(ctx, g.database, g.registry, operation)
			if commandErr != nil {
				return fmt.Errorf("advance subscription after accepted billing: %s", commandErr.Message)
			}
			if commandResult.Error != nil {
				return fmt.Errorf("advance subscription after accepted billing: %s", commandResult.Error.Message)
			}
			subscription, err = tx.GetDoc(subscriptionType, name, "")
			if err != nil {
				return fmt.Errorf("reload advanced subscription: %w", err)
			}
			continue
		}
		scheduleData, err := json.Marshal(map[string]any{
			"reference": reference, "subscription": subscription.Name,
			"source_record": subscription.Name, "actor": "kora:recurring-invoice-scheduler",
			"billing_date": billingDate, "period_start": billingDate, "period_end": periodEnd,
			"amount": amount, "currency": subscription.GetString("currency"), "status": "Invoiced",
		})
		if err != nil {
			return err
		}
		dueDate := due.Add(dueDateOffset).Format("2006-01-02")
		invoiceData, err := json.Marshal(map[string]any{
			"reference": reference, "source_record": subscription.Name,
			"invoice_number": recurringInvoiceNumber(subscription.Name, due),
			"subscription":   subscription.Name, "customer": subscription.GetString("customer"),
			"billing_schedule": "$records.schedule.name", "issue_date": billingDate, "due_date": dueDate,
			"subtotal": subtotal, "tax_amount": taxAmount, "total": amount, "amount_paid": 0, "balance": amount,
			"currency": subscription.GetString("currency"), "status": "Draft",
		})
		if err != nil {
			return err
		}
		advanceData, err := json.Marshal(map[string]any{"next_billing_date": next.Format("2006-01-02")})
		if err != nil {
			return err
		}
		payload, err := json.Marshal(kernel.RecordMutationBundlePayload{
			Doctype: "Billing Schedule",
			Records: []kernel.RecordMutationBundleItem{
				{Key: "schedule", Doctype: "Billing Schedule", Data: scheduleData},
				{Key: "invoice", Doctype: "Recurring Invoice", Data: invoiceData},
				{Key: "advance", Operation: "update", Doctype: "Subscription", Name: subscription.Name, ExpectedRevision: subscription.Revision, Data: advanceData},
			},
		})
		if err != nil {
			return err
		}
		key := recurringInvoiceIdempotencyKey(subscription.Name, billingDate)
		operation := kernel.Operation{
			Command: kernel.CommandRecordMutateBundle, Payload: payload,
			Context: kernel.OperationContext{
				Site: g.siteName, TenantID: execution.Tenant(), Execution: execution,
				Source: kernel.SourceWorker, User: "system", IdempotencyKey: key,
				ConfigRevision: g.configRevision,
			},
		}
		result, kernelErr := g.kernel.Execute(ctx, g.database, g.registry, operation)
		if kernelErr != nil {
			return fmt.Errorf("create invoice for %s: %s", billingDate, kernelErr.Message)
		}
		if result.Error != nil {
			return fmt.Errorf("create invoice for %s: %s", billingDate, result.Error.Message)
		}
		// Reload the revision and advance date committed by the bundle before
		// processing another overdue period.
		subscription, err = tx.GetDoc(subscriptionType, name, "")
		if err != nil {
			return fmt.Errorf("reload advanced subscription: %w", err)
		}
	}
	return nil
}

func configuredDueDays(config map[string]any) (int, error) {
	if config == nil || config["due_days"] == nil {
		return 0, nil // due on issue date unless explicitly configured otherwise
	}
	var days int
	switch value := config["due_days"].(type) {
	case int:
		days = value
	case int64:
		days = int(value)
	case float64:
		if value != math.Trunc(value) {
			return 0, errors.New("subscription_invoice config due_days must be a whole number")
		}
		days = int(value)
	case string:
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, errors.New("subscription_invoice config due_days must be an integer")
		}
		days = parsed
	default:
		return 0, errors.New("subscription_invoice config due_days must be an integer")
	}
	if days < 0 || days > 365 {
		return 0, errors.New("subscription_invoice config due_days must be between 0 and 365")
	}
	return days, nil
}

func (g *recurringInvoiceGenerator) subscriptionTotals(tx *orm.TxManager, subscription *doctype.Document) (subtotal, taxAmount, total float64, err error) {
	quantity := subscription.GetFloat("quantity")
	if quantity == 0 {
		quantity = 1
	}
	subtotal = subscription.GetFloat("unit_price") * quantity
	lineType := g.registry.Get("Subscription Line")
	if lineType != nil {
		filters, _ := json.Marshal([][]any{{"subscription", "=", subscription.Name}, {"status", "=", "Active"}})
		lines, total, err := tx.GetListWithOptions(lineType, string(filters), "name ASC", 501, 0, "", []string{"quantity", "unit_price"}, "")
		if err != nil {
			return 0, 0, 0, fmt.Errorf("list active subscription lines: %w", err)
		}
		if total > 500 {
			return 0, 0, 0, fmt.Errorf("subscription %s has more than 500 active lines", subscription.Name)
		}
		for _, line := range lines {
			quantity := line.GetFloat("quantity")
			if quantity == 0 {
				quantity = 1
			}
			subtotal += line.GetFloat("unit_price") * quantity
		}
	}
	var taxRate float64
	planType := g.registry.Get("Plan")
	if planType != nil && subscription.GetString("plan") != "" {
		plan, err := tx.GetDoc(planType, subscription.GetString("plan"), "")
		if err != nil {
			return 0, 0, 0, fmt.Errorf("load subscription plan: %w", err)
		}
		taxRate = plan.GetFloat("tax_rate")
	}
	subtotal = math.Round(subtotal*100) / 100
	taxAmount = math.Round(subtotal*taxRate) / 100
	total = math.Round((subtotal+taxAmount)*100) / 100
	return subtotal, taxAmount, total, nil
}

func recurringInvoiceNumber(subscription string, billingDate time.Time) string {
	return subscription + "-" + billingDate.Format("20060102")
}

func recurringInvoiceIdempotencyKey(subscription, billingDate string) string {
	digest := sha256.Sum256([]byte(subscription + "\x00" + billingDate))
	return "recurring-invoice-" + hex.EncodeToString(digest[:])
}

func nextSubscriptionBillingDate(current, start time.Time, interval string, count int) (time.Time, error) {
	if count < 1 {
		return time.Time{}, errors.New("subscription interval_count must be positive")
	}
	switch interval {
	case "Weekly":
		return current.AddDate(0, 0, 7*count), nil
	case "Monthly", "Quarterly", "Annually":
		months := count
		if interval == "Quarterly" {
			months *= 3
		} else if interval == "Annually" {
			months *= 12
		}
		monthOffset := (current.Year()-start.Year())*12 + int(current.Month()-start.Month()) + months
		firstOfTarget := time.Date(start.Year(), start.Month()+time.Month(monthOffset), 1, 0, 0, 0, 0, time.UTC)
		lastDay := time.Date(firstOfTarget.Year(), firstOfTarget.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
		day := start.Day()
		if day > lastDay {
			day = lastDay
		}
		return time.Date(firstOfTarget.Year(), firstOfTarget.Month(), day, 0, 0, 0, 0, time.UTC), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported subscription billing interval %q", interval)
	}
}
