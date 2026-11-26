//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/kernel"
)

const creditPackDir = "../../config/_parked/erpsell"

// creditSaleBody builds a Sale request with one product line.
func creditSaleBody(status, customer, product string, qty, unit float64, extra map[string]any) string {
	body := map[string]any{
		"sale_date": "2026-11-20", "status": status,
		"items": []any{map[string]any{"product": product, "quantity": qty, "unit_price": unit}},
	}
	if customer != "" {
		body["customer"] = customer
	}
	for key, value := range extra {
		body[key] = value
	}
	return jsonBodyForCredit(body)
}

// creditPaymentBody builds a Payment request linked to sale.
func creditPaymentBody(sale, status string, amount float64, extra map[string]any) string {
	body := map[string]any{"sale": sale, "status": status, "amount": amount}
	for key, value := range extra {
		body[key] = value
	}
	return jsonBodyForCredit(body)
}

// jsonBodyForCredit encodes a request body made only of strings and numbers, which cannot fail to encode.
func jsonBodyForCredit(body map[string]any) string {
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// creditSeed creates a customer and a product with 10 in stock at 100.
func creditSeed(f *packFixture) (customer, product string) {
	customer = f.create("Customer", jsonBody(f.t, map[string]any{"customer_name": "Asha"}))
	product = f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Tea", "selling_price": 100, "cost_price": 40, "stock_qty": 10}))
	return customer, product
}

// creditKernelCommand runs a pack command through the kernel as the fixture's actor, the way
// the register's tender panel submits record_sale and hold_sale. Owner-module commands are not
// reachable through the public kernel endpoint, so this drives the kernel with the site's own
// transaction manager, the one a published operation runs with, script runner included.
func creditKernelCommand(f *packFixture, command string, data map[string]any) (int, string) {
	f.t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/kernel/"+command, nil)
	execution := integrationTestExecution(f.t, f.site, "admin@example.test", f.roles)
	actor := execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	role := ""
	if len(actor.Roles) > 0 {
		role = actor.Roles[0]
	}
	ctx.Set("site_name", f.site)
	ctx.Set("site_db", f.db)
	ctx.Set("site_registry", f.reg)
	ctx.Set("site_db_type", f.dialect)
	ctx.Set("user", user)
	ctx.Set("user_role", role)
	ctx.Set("user_roles", append([]string(nil), actor.Roles...))
	ctx.Set("policy_execution", execution)

	services := f.handler.runtimeService(ctx, f.site)
	k := kernel.New(f.handler.siteDialect(ctx), f.handler.siteOutboxWriter(ctx))
	k.TxManager = f.handler.siteTx(ctx)
	k.Commands = f.handler.kernelCommands(services)
	k.CommandSchemas = services.CommandSchemas
	payload := jsonBody(f.t, map[string]any{"data": data})
	op := kernel.Operation{
		Context: kernel.OperationContext{
			Site: f.site, TenantID: execution.Tenant(), Execution: execution, User: user, Roles: actor.Roles,
			UserRole: role, Actor: actor, Source: kernel.SourceIntegration, IdempotencyKey: fmt.Sprintf("credit-%s-%d", command, time.Now().UnixNano()),
		},
		Command: command, Payload: []byte(payload), Deadline: time.Now().Add(30 * time.Second),
	}
	result, cerr := k.Execute(ctx.Request.Context(), f.db, f.reg, op)
	if cerr != nil || result.Status != contract.StatusCompleted {
		return http.StatusBadRequest, fmt.Sprintf("%+v", cerr)
	}
	return http.StatusOK, string(result.Data)
}

// creditMoney expects a money column to hold want, to the cent.
func creditMoney(f *packFixture, recordType, name, column string, want float64) {
	f.t.Helper()
	if got := f.amount(recordType, name, column); math.Abs(got-want) > 0.005 {
		f.t.Fatalf("%s %s.%s = %v, want %v", recordType, name, column, got, want)
	}
}

// creditFlag expects a Check column to be set or not (MySQL stores 1, PostgreSQL true).
func creditFlag(f *packFixture, recordType, name, column string, want bool) {
	f.t.Helper()
	got := f.value(recordType, name, column)
	if on := got == "1" || got == "true"; on != want {
		f.t.Fatalf("%s %s.%s = %q, want on=%v", recordType, name, column, got, want)
	}
}

// creditCashIntent seeds a confirmed cash payment intent for one cash line of record_sale.
// The register's cash lines carry such an intent as their reference, and the kernel
// consumes it once.
func creditCashIntent(f *packFixture, n int, amount float64) string {
	f.t.Helper()
	id := fmt.Sprintf("CASHINTENTSALE%012d", n)
	stamp := time.Now().UTC()
	f.exec("INSERT INTO _kora_payment_intent (id, site, provider, state, amount, currency, source_doctype, idempotency_key, payload_hash, actor_user, created_at, updated_at) VALUES (?, ?, 'cash', 'confirmed', ?, '', 'Sale', ?, 'erpsell-test', 'admin@example.test', ?, ?)",
		id, f.site, fmt.Sprint(amount), id, stamp, stamp)
	return id
}

// creditCommandRegistry loads the pack's command definitions as the kernel runs them. Owner
// commands need their module's published schemas, which this fixture does not install, so the
// owner is cleared. The command steps (upsert, create, update) and their script hooks run the same way.
func creditCommandRegistry(t *testing.T) *kernel.CommandRegistry {
	t.Helper()
	defs, err := kernel.ParseCommandDir(creditPackDir + "/commands")
	if err != nil {
		t.Fatalf("parse erpsell commands: %v", err)
	}
	reg := kernel.NewCommandRegistry()
	for _, def := range defs {
		def.ModuleOwnerID = ""
		if err := reg.Register(def); err != nil {
			t.Fatalf("register %s: %v", def.FullName(), err)
		}
	}
	return reg
}

// TestPackScriptsSellingCredit proves the credit-sale rules, the register's paths (a sale
// created as Paid, and a held sale paid later), and that the scripts run on command steps.
func TestPackScriptsSellingCredit(t *testing.T) {
	eachPackDialect(t, []string{creditPackDir}, func(f *packFixture) {
		usd := f.create("Currency", jsonBody(f.t, map[string]any{"code": "USD", "currency_name": "US Dollar", "rate_to_base": 130}))

		f.run("credit sale needs a customer", func(f *packFixture) {
			_, product := creditSeed(f)
			f.refused(httpMethodPost, "Sale", "", creditSaleBody("Awaiting Payment", "", product, 3, 100, nil), "A sale on credit needs a customer.")
		})

		f.run("credit sale moves the customer balance and is paid by recording payments", func(f *packFixture) {
			customer, product := creditSeed(f)
			other := f.create("Customer", jsonBody(f.t, map[string]any{"customer_name": "Bayo"}))
			sale := f.create("Sale", creditSaleBody("Awaiting Payment", customer, product, 3, 100, nil))
			creditFlag(f, "Sale", sale, "on_credit", true)
			creditMoney(f, "Sale", sale, "base_outstanding", 300)
			creditMoney(f, "Customer", customer, "outstanding_balance", 300)
			f.refused(httpMethodPut, "Sale", sale, `{"status":"Paid"}`, "A sale on credit is paid by recording payments.")

			f.create("Payment", creditPaymentBody(sale, "Received", 100, nil))
			f.expect("Sale", sale, "status", "Partially Paid")
			creditMoney(f, "Sale", sale, "base_amount_paid", 100)
			creditMoney(f, "Customer", customer, "outstanding_balance", 200)
			f.refused(httpMethodPut, "Sale", sale, fmt.Sprintf(`{"customer":%q}`, other), "its total, currency and customer cannot change")

			// Two currencies settle one sale: 1.50 USD at 130 is 195 in the base currency.
			f.create("Payment", creditPaymentBody(sale, "Received", 1.5, map[string]any{"currency": usd, "exchange_rate": 130, "method": "Card"}))
			creditMoney(f, "Sale", sale, "base_amount_paid", 295)
			creditMoney(f, "Sale", sale, "base_outstanding", 5)
			creditMoney(f, "Customer", customer, "outstanding_balance", 5)

			f.refused(httpMethodPost, "Payment", "", creditPaymentBody(sale, "Received", 5.02, nil), "more than the 5 still owed")
			f.create("Payment", creditPaymentBody(sale, "Received", 5, nil))
			f.expect("Sale", sale, "status", "Paid")
			creditMoney(f, "Sale", sale, "base_outstanding", 0)
			creditMoney(f, "Customer", customer, "outstanding_balance", 0)
		})

		f.run("a voided payment reverses and a voided payment is final", func(f *packFixture) {
			customer, product := creditSeed(f)
			sale := f.create("Sale", creditSaleBody("Awaiting Payment", customer, product, 3, 100, nil))
			payment := f.create("Payment", creditPaymentBody(sale, "Received", 100, nil))
			f.refused(httpMethodPut, "Sale", sale, `{"status":"Void"}`, "Void its payments first.")
			f.update("Payment", payment, `{"status":"Voided"}`)
			f.expect("Sale", sale, "status", "Awaiting Payment")
			creditMoney(f, "Sale", sale, "base_amount_paid", 0)
			creditMoney(f, "Customer", customer, "outstanding_balance", 300)
			f.refused(httpMethodPut, "Payment", payment, `{"status":"Received"}`, "is voided and cannot change")
		})

		f.run("a received payment cannot go pending and cannot be deleted", func(f *packFixture) {
			customer, product := creditSeed(f)
			sale := f.create("Sale", creditSaleBody("Awaiting Payment", customer, product, 3, 100, nil))
			payment := f.create("Payment", creditPaymentBody(sale, "Received", 100, nil))
			f.refused(httpMethodPut, "Payment", payment, `{"status":"Pending"}`, "void it instead")
			f.refused(httpMethodDelete, "Payment", payment, "", "void it instead of deleting it")
		})

		f.run("cancelling a credit sale without payments removes its balance", func(f *packFixture) {
			customer, product := creditSeed(f)
			sale := f.create("Sale", creditSaleBody("Awaiting Payment", customer, product, 3, 100, nil))
			creditMoney(f, "Customer", customer, "outstanding_balance", 300)
			f.update("Sale", sale, `{"status":"Cancelled"}`)
			creditMoney(f, "Customer", customer, "outstanding_balance", 0)
		})

		f.run("a credit sale is recorded by a cashier, and the cashier cannot mark it paid by hand", func(f *packFixture) {
			customer, product := creditSeed(f)
			cashier := f.as("Cashier")
			sale := cashier.create("Sale", creditSaleBody("Awaiting Payment", customer, product, 3, 100, nil))
			creditMoney(f, "Customer", customer, "outstanding_balance", 300)
			cashier.refused(httpMethodPut, "Sale", sale, `{"status":"Paid"}`, "A sale on credit is paid by recording payments.")
			cashier.create("Payment", creditPaymentBody(sale, "Received", 100, nil))
			f.expect("Sale", sale, "status", "Partially Paid")
			creditMoney(f, "Customer", customer, "outstanding_balance", 200)
		})

		f.run("a draft entered as Awaiting Payment becomes a credit sale and owes its customer", func(f *packFixture) {
			customer, product := creditSeed(f)
			sale := f.create("Sale", creditSaleBody("Draft", customer, product, 3, 100, nil))
			creditFlag(f, "Sale", sale, "on_credit", false)
			creditMoney(f, "Customer", customer, "outstanding_balance", 0)
			f.update("Sale", sale, `{"status":"Awaiting Payment"}`)
			creditFlag(f, "Sale", sale, "on_credit", true)
			creditMoney(f, "Sale", sale, "base_outstanding", 300)
			creditMoney(f, "Customer", customer, "outstanding_balance", 300)
			f.create("Payment", creditPaymentBody(sale, "Received", 100, nil))
			f.expect("Sale", sale, "status", "Partially Paid")
			creditMoney(f, "Customer", customer, "outstanding_balance", 200)
		})

		f.run("a draft cannot become a credit sale without a customer", func(f *packFixture) {
			_, product := creditSeed(f)
			sale := f.create("Sale", creditSaleBody("Draft", "", product, 3, 100, nil))
			f.refused(httpMethodPut, "Sale", sale, `{"status":"Awaiting Payment"}`, "A sale on credit needs a customer.")
			creditFlag(f, "Sale", sale, "on_credit", false)
		})

		f.run("a payment on a register sale changes nothing", func(f *packFixture) {
			customer, product := creditSeed(f)
			sale := f.create("Sale", creditSaleBody("Paid", customer, product, 3, 100, map[string]any{
				"payments": []any{map[string]any{"method": "Cash", "amount": 300}},
			}))
			creditMoney(f, "Sale", sale, "base_amount_paid", 300)
			creditMoney(f, "Sale", sale, "base_outstanding", 0)
			creditMoney(f, "Customer", customer, "outstanding_balance", 0)
			f.create("Payment", creditPaymentBody(sale, "Received", 50, nil))
			creditMoney(f, "Sale", sale, "base_amount_paid", 300)
			f.expect("Sale", sale, "status", "Paid")
		})

		f.run("register sale created as Paid is paid in full and moves stock", func(f *packFixture) {
			customer, product := creditSeed(f)
			sale := f.create("Sale", creditSaleBody("Paid", customer, product, 3, 100, map[string]any{
				"payments": []any{map[string]any{"method": "Cash", "amount": 300}},
			}))
			creditFlag(f, "Sale", sale, "on_credit", false)
			creditMoney(f, "Sale", sale, "base_amount_paid", 300)
			creditMoney(f, "Sale", sale, "base_outstanding", 0)
			creditMoney(f, "Customer", customer, "outstanding_balance", 0)
			if got := f.amount("Product", product, "stock_qty"); got != 7 {
				f.t.Fatalf("stock after a Paid sale of 3 = %v, want 7", got)
			}
		})

		f.run("held register sale paid at the till is paid in full on update", func(f *packFixture) {
			customer, product := creditSeed(f)
			sale := f.create("Sale", creditSaleBody("Held", customer, product, 2, 100, nil))
			creditMoney(f, "Sale", sale, "base_amount_paid", 0)
			creditMoney(f, "Sale", sale, "base_outstanding", 200)
			if got := f.amount("Product", product, "stock_qty"); got != 10 {
				f.t.Fatalf("stock while held = %v, want 10", got)
			}
			f.update("Sale", sale, `{"status":"Paid","payments":[{"method":"Cash","amount":200}]}`)
			creditMoney(f, "Sale", sale, "base_amount_paid", 200)
			creditMoney(f, "Sale", sale, "base_outstanding", 0)
			creditMoney(f, "Customer", customer, "outstanding_balance", 0)
			if got := f.amount("Product", product, "stock_qty"); got != 8 {
				f.t.Fatalf("stock after paying the held sale = %v, want 8", got)
			}
		})

		f.run("register commands run the sale scripts on create and on update", func(f *packFixture) {
			customer, product := creditSeed(f)
			reg := creditCommandRegistry(f.t)
			finish, err := kernel.ParseCommandResource([]byte(creditFinishSaleYAML))
			if err != nil {
				f.t.Fatalf("parse test finish command: %v", err)
			}
			if err := reg.Register(finish); err != nil {
				f.t.Fatalf("register test finish command: %v", err)
			}
			f.handler.KernelCommands = reg
			for _, stmt := range creditPaymentTables(f.dialect) {
				if _, err := f.db.Exec(stmt); err != nil {
					f.t.Fatalf("create payment intent table: %v", err)
				}
			}
			till := f.create("Till Session", jsonBody(f.t, map[string]any{"status": "Open", "register_code": "REG-001"}))
			line := []any{map[string]any{"product": product, "quantity": 3, "unit_price": 100}}

			// record_sale as the tender panel sends a cash sale without a preset number: one Paid sale.
			intent := creditCashIntent(f, 1, 300)
			code, body := creditKernelCommand(f, "pos.record_sale", map[string]any{
				"till_session": till, "register": "REG-001", "location": "Main",
				"customer": customer, "currency": "", "items": line,
				"payments": []any{map[string]any{"method": "Cash", "amount": 300, "currency": "", "reference": intent}},
			})
			if code != http.StatusOK {
				f.t.Fatalf("record_sale = %d: %s", code, body)
			}
			sale := creditFirstStepName(f, body)
			creditFlag(f, "Sale", sale, "on_credit", false)
			creditMoney(f, "Sale", sale, "base_amount_paid", 300)
			creditMoney(f, "Sale", sale, "base_outstanding", 0)
			creditMoney(f, "Customer", customer, "outstanding_balance", 0)
			if got := f.amount("Product", product, "stock_qty"); got != 7 {
				f.t.Fatalf("stock after record_sale of 3 = %v, want 7", got)
			}

			// A sale saved as Draft-then-paid through a command update: its payments settle it in full.
			held := f.create("Sale", creditSaleBody("Held", customer, product, 2, 100, nil))
			creditMoney(f, "Sale", held, "base_amount_paid", 0)
			code, body = creditKernelCommand(f, "pos.finish_sale", map[string]any{
				"sale": held, "payments": []any{map[string]any{"method": "Cash", "amount": 200}},
			})
			if code != http.StatusOK {
				f.t.Fatalf("finish_sale = %d: %s", code, body)
			}
			f.expect("Sale", held, "status", "Paid")
			creditMoney(f, "Sale", held, "base_amount_paid", 200)
			creditMoney(f, "Sale", held, "base_outstanding", 0)
			creditMoney(f, "Customer", customer, "outstanding_balance", 0)
			if got := f.amount("Product", product, "stock_qty"); got != 5 {
				f.t.Fatalf("stock after paying the held sale = %v, want 5", got)
			}
		})
	})
}

// creditFinishSaleYAML is a test-only command that updates an existing sale to Paid
// with its payments. It exercises the command update step, which runs the sale's hooks.
const creditFinishSaleYAML = `
name: finish_sale
namespace: pos
version: 1
input:
  record: Sale
  extra:
    sale: {type: string, required: true}
transaction:
  - update:
      record: Sale
      name: $input.sale
      values: {status: Paid, payments: $input.payments}
`

// creditPaymentTables returns the payment intent DDL for the fixture's dialect.
func creditPaymentTables(dialect string) []string {
	if dialect == "postgres" {
		return kdb.PaymentTablesPostgres()
	}
	return kdb.PaymentTablesMySQL()
}

// creditFirstStepName returns the name of the record the first step of a command wrote.
func creditFirstStepName(f *packFixture, body string) string {
	f.t.Helper()
	var out struct {
		Steps []struct {
			Name string `json:"name"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Steps) == 0 {
		f.t.Fatalf("read command steps from %s: %v", body, err)
	}
	return out.Steps[0].Name
}
