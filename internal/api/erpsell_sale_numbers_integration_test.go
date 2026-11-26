//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/kernel"
)

const saleNumberPackDir = "../../config/_parked/erpsell"

// saleNumberCommand runs one erp-sell command through the kernel as the fixture's
// Administrator, with the site's own transaction manager, the way a published operation
// runs. Owner-module commands are cleared of their owner by saleNumberRegistry.
func saleNumberCommand(f *packFixture, command string, data map[string]any) (int, string) {
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
			UserRole: role, Actor: actor, Source: kernel.SourceIntegration,
			IdempotencyKey: fmt.Sprintf("sale-number-%s-%d", command, time.Now().UnixNano()),
		},
		Command: command, Payload: []byte(payload), Deadline: time.Now().Add(30 * time.Second),
	}
	result, cerr := k.Execute(ctx.Request.Context(), f.db, f.reg, op)
	if cerr != nil || result.Status != contract.StatusCompleted {
		return http.StatusBadRequest, fmt.Sprintf("%+v", cerr)
	}
	return http.StatusOK, string(result.Data)
}

// saleNumberOutputReference returns the output reference of a command envelope.
func saleNumberOutputReference(f *packFixture, body string) string {
	f.t.Helper()
	var out struct {
		Output struct {
			Reference string `json:"reference"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Output.Reference == "" {
		f.t.Fatalf("read output reference from %s: %v", body, err)
	}
	return out.Output.Reference
}

// saleNumberRegistry loads the pack's command definitions with their owner cleared, as the
// fixture cannot install the owner module's published schemas.
func saleNumberRegistry(t *testing.T) *kernel.CommandRegistry {
	t.Helper()
	defs, err := kernel.ParseCommandDir(saleNumberPackDir + "/commands")
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

// saleNumberCashIntent seeds a confirmed cash payment intent for one cash line of record_sale.
func saleNumberCashIntent(f *packFixture, n int, amount float64) string {
	f.t.Helper()
	id := fmt.Sprintf("SALENUMBERCASH%010d", n)
	stamp := time.Now().UTC()
	f.exec("INSERT INTO _kora_payment_intent (id, site, provider, state, amount, currency, source_doctype, idempotency_key, payload_hash, actor_user, created_at, updated_at) VALUES (?, ?, 'cash', 'confirmed', ?, '', 'Sale', ?, 'sale-number-test', 'admin@example.test', ?, ?)",
		id, f.site, fmt.Sprint(amount), id, stamp, stamp)
	return id
}

// saleNumberPaymentTables returns the payment intent DDL for the fixture's dialect.
func saleNumberPaymentTables(dialect string) []string {
	if dialect == "postgres" {
		return kdb.PaymentTablesPostgres()
	}
	return kdb.PaymentTablesMySQL()
}

// TestPackScriptsSaleNumbers proves the erp-sell Sale naming series: a till reserves a block
// of sale numbers in the SALE-<year>- series, holds a sale under a reserved number, records
// that sale as Paid under the same number, and a plain created sale takes the next series name.
func TestPackScriptsSaleNumbers(t *testing.T) {
	eachPackDialect(t, []string{saleNumberPackDir}, func(f *packFixture) {
		f.run("reserve a block, hold and pay a sale under a reserved number, then a plain sale", func(f *packFixture) {
			f.handler.KernelCommands = saleNumberRegistry(f.t)
			for _, stmt := range saleNumberPaymentTables(f.dialect) {
				if _, err := f.db.Exec(stmt); err != nil {
					f.t.Fatalf("create payment intent table: %v", err)
				}
			}
			series := regexp.MustCompile(`^SALE-` + strconv.Itoa(time.Now().UTC().Year()) + `-[0-9]{5}$`)
			product := f.create("Product", jsonBody(f.t, map[string]any{"product_name": "Tea", "selling_price": 100, "cost_price": 40, "stock_qty": 10}))
			register := f.create("Register", jsonBody(f.t, map[string]any{"register_name": "Till One", "reference": "REG-SALE-1", "location": "Main"}))
			lines := []any{map[string]any{"product": product, "quantity": 2, "unit_price": 100}}

			code, body := saleNumberCommand(f, "pos.open_session", map[string]any{
				"register": register, "location": "Main", "opening_float": 0,
			})
			if code != http.StatusOK {
				f.t.Fatalf("open_session = %d: %s", code, body)
			}
			till := saleNumberOutputReference(f, body)
			f.expect("Till Session", till, "status", "Open")

			code, body = saleNumberCommand(f, "pos.reserve_numbers", map[string]any{"till_session": till})
			if code != http.StatusOK {
				f.t.Fatalf("reserve_numbers = %d: %s", code, body)
			}
			reserved := saleNumberOutputReference(f, body)
			if !series.MatchString(reserved) {
				f.t.Fatalf("reserved number %q is not in the SALE-<year>- series", reserved)
			}

			code, body = saleNumberCommand(f, "pos.hold_sale", map[string]any{
				"sale_number": reserved, "till_session": till, "register": register, "location": "Main", "items": lines,
			})
			if code != http.StatusOK {
				f.t.Fatalf("hold_sale = %d: %s", code, body)
			}
			if got := saleNumberOutputReference(f, body); got != reserved {
				f.t.Fatalf("hold_sale reference = %q, want the reserved %q", got, reserved)
			}
			f.expect("Sale", reserved, "status", "Held")

			intent := saleNumberCashIntent(f, 1, 200)
			code, body = saleNumberCommand(f, "pos.record_sale", map[string]any{
				"sale_number": reserved, "till_session": till, "register": register, "location": "Main",
				"currency": "", "items": lines,
				"payments": []any{map[string]any{"method": "Cash", "amount": 200, "currency": "", "reference": intent}},
			})
			if code != http.StatusOK {
				f.t.Fatalf("record_sale = %d: %s", code, body)
			}
			if got := saleNumberOutputReference(f, body); got != reserved {
				f.t.Fatalf("record_sale reference = %q, want the held %q", got, reserved)
			}
			f.expect("Sale", reserved, "status", "Paid")
			if got := f.amount("Till Session", till, "cash_sale_total"); got != 200 {
				f.t.Fatalf("till %s cash_sale_total = %v, want 200 from the paid sale", till, got)
			}

			plain := f.create("Sale", jsonBody(f.t, map[string]any{
				"sale_date": "2026-11-20", "status": "Paid", "items": lines,
				"payments": []any{map[string]any{"method": "Cash", "amount": 200}},
			}))
			if !series.MatchString(plain) {
				f.t.Fatalf("plain sale name %q is not in the SALE-<year>- series", plain)
			}
			if plain == reserved {
				f.t.Fatalf("plain sale reused the reserved number %q", reserved)
			}
		})
	})
}
