//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/kernel"
)

// A Sale's quantity_effect is gated by its status: it takes stock while the
// status is Awaiting Payment, Partially Paid or Paid, and gives it back when the
// sale leaves that set. These run on MySQL and Postgres through the REST API.

func saleLines(qty float64) []map[string]any {
	return []map[string]any{{"product": "SODA", "quantity": qty}}
}

// createdName reads the name of the record a create response returned.
func createdName(t *testing.T, body string) string {
	t.Helper()
	var doc struct{ Data map[string]any }
	if err := json.Unmarshal([]byte(body), &doc); err != nil || doc.Data["name"] == nil {
		t.Fatalf("create response has no name: %s", body)
	}
	return fmt.Sprint(doc.Data["name"])
}

func (e *effectHarness) saleStatus(t *testing.T, name string) string {
	t.Helper()
	var status string
	if err := e.db.QueryRow(kdb.Rebind(e.dialect, "SELECT status FROM "+e.table("Sale")+" WHERE name = ?"), name).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func (e *effectHarness) setStatus(t *testing.T, name, status string) {
	t.Helper()
	rec := e.put(t, "/api/resource/Sale/"+name, fmt.Sprintf(`{"status":%q}`, status), effectAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("moving sale to %s: status = %d: %s", status, rec.Code, rec.Body.String())
	}
}

func TestDraftSaleChangesNoStockAndItsLinesStayFree(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		code, body := e.post(t, "Sale", map[string]any{"reference": "SALE-1", "items": saleLines(3)}, "", effectAdmin)
		if code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		if got := e.stockOf(t, "SODA"); got != 10 {
			t.Fatalf("a draft sale moved stock to %v, want 10", got)
		}
		name := createdName(t, body)
		rec := e.put(t, "/api/resource/Sale/"+name, `{"items":[{"product":"SODA","quantity":4}]}`, effectAdmin)
		if rec.Code != http.StatusOK || e.stockOf(t, "SODA") != 10 {
			t.Fatalf("editing a draft: status %d, stock %v: %s", rec.Code, e.stockOf(t, "SODA"), rec.Body.String())
		}
	})
}

func TestDraftSaleMovesStockWhenItBecomesPaid(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		e.setStock(t, "WATER", 5)
		code, body := e.post(t, "Sale", map[string]any{"reference": "SALE-2", "items": []map[string]any{
			{"product": "SODA", "quantity": 3}, {"product": "WATER", "quantity": 2},
		}}, "", effectAdmin)
		if code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		e.setStatus(t, createdName(t, body), "Paid")
		if soda, water := e.stockOf(t, "SODA"), e.stockOf(t, "WATER"); soda != 7 || water != 3 {
			t.Fatalf("stock soda=%v water=%v, want 7 and 3", soda, water)
		}
	})
}

func TestCancellingOrRefundingAPaidSaleGivesTheStockBack(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		for _, to := range []string{"Cancelled", "Refunded"} {
			e.setStock(t, "SODA", 10)
			code, body := e.post(t, "Sale", map[string]any{"reference": "SALE-" + to, "status": "Paid", "items": saleLines(3)}, "", effectAdmin)
			if code != http.StatusCreated {
				t.Fatalf("%s: status = %d: %s", to, code, body)
			}
			name := createdName(t, body)
			if got := e.stockOf(t, "SODA"); got != 7 {
				t.Fatalf("%s: stock after the paid sale = %v, want 7", to, got)
			}
			e.setStatus(t, name, to)
			if got := e.stockOf(t, "SODA"); got != 10 {
				t.Fatalf("%s: stock after the sale was %s = %v, want 10 restored", to, to, got)
			}
		}
	})
}

func TestSaleCreatedAsPaidDecreasesStockOnce(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		if code, body := e.post(t, "Sale", map[string]any{"reference": "SALE-3", "status": "Paid", "items": saleLines(3)}, "", effectAdmin); code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		if got := e.stockOf(t, "SODA"); got != 7 {
			t.Fatalf("stock = %v, want 7", got)
		}
	})
}

func TestRetryOfAPaidSaleWithTheSameKeyDoesNotChangeStockTwice(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		body := map[string]any{"reference": "SALE-4", "status": "Paid", "items": saleLines(3)}
		for i := 0; i < 2; i++ {
			if code, out := e.post(t, "Sale", body, "sale-key-4", effectAdmin); code != http.StatusCreated {
				t.Fatalf("attempt %d status = %d: %s", i+1, code, out)
			}
		}
		if got := e.stockOf(t, "SODA"); got != 7 {
			t.Fatalf("stock = %v after a retry, want 7 (taken once)", got)
		}
		if n := e.countOf(t, "Sale"); n != 1 {
			t.Fatalf("sales = %d, want 1", n)
		}
	})
}

// Moves inside the applying set (awaiting payment, part paid, paid) take no
// further stock, and repeating the same status changes nothing.
func TestMovesWithinTheApplyingStatusesChangeStockOnlyOnce(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		code, body := e.post(t, "Sale", map[string]any{"reference": "SALE-5", "items": saleLines(3)}, "", effectAdmin)
		if code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		name := createdName(t, body)
		for _, status := range []string{"Awaiting Payment", "Partially Paid", "Paid", "Paid"} {
			e.setStatus(t, name, status)
			if got := e.stockOf(t, "SODA"); got != 7 {
				t.Fatalf("after %s: stock = %v, want 7", status, got)
			}
		}
	})
}

func TestLinesCannotChangeWhileTheSaleIsApplied(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		code, body := e.post(t, "Sale", map[string]any{"reference": "SALE-6", "status": "Paid", "items": saleLines(3)}, "", effectAdmin)
		if code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		name := createdName(t, body)
		rec := e.put(t, "/api/resource/Sale/"+name, `{"status":"Paid","items":[{"product":"SODA","quantity":5}]}`, effectAdmin)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("changing the lines of a paid sale: status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if got := e.stockOf(t, "SODA"); got != 7 {
			t.Fatalf("stock = %v, want 7 (the refused edit must change nothing)", got)
		}
	})
}

func TestSaleThatCannotBeFilledIsRefusedAndStaysDraft(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		code, body := e.post(t, "Sale", map[string]any{"reference": "SALE-7", "items": saleLines(11)}, "", effectAdmin)
		if code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		name := createdName(t, body)
		rec := e.put(t, "/api/resource/Sale/"+name, `{"status":"Paid"}`, effectAdmin)
		if rec.Code == http.StatusOK {
			t.Fatalf("paying for more than is in stock was accepted: %s", rec.Body.String())
		}
		if got := e.stockOf(t, "SODA"); got != 10 {
			t.Fatalf("stock = %v, want 10", got)
		}
		if status := e.saleStatus(t, name); status != "Draft" {
			t.Fatalf("status = %q after a refused payment, want Draft", status)
		}
	})
}

// runBundle sends a record bundle the way a register checkout does: one kernel
// operation that creates several records together.
func (e *effectHarness) runBundle(t *testing.T, payload kernel.RecordMutationBundlePayload, key string) *contract.Error {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/view/action/complete", nil)
	execution := integrationTestExecution(t, effectSite, "admin@example.test", effectAdmin)
	ctx.Set("site_name", effectSite)
	ctx.Set("site_db", e.db)
	ctx.Set("site_registry", e.reg)
	ctx.Set("site_db_type", e.driver)
	ctx.Set("user", execution.Actor().PrincipalID)
	ctx.Set("user_role", effectAdmin[0])
	ctx.Set("user_roles", append([]string(nil), effectAdmin...))
	ctx.Set("policy_execution", execution)
	_, _, cerr := e.h.runKernelMutationBundle(ctx, payload, key)
	return cerr
}

func TestSaleCreatedAsPaidInABundleDecreasesStockOnce(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		data, _ := json.Marshal(map[string]any{"reference": "SALE-B1", "status": "Paid", "items": saleLines(2)})
		payload := kernel.RecordMutationBundlePayload{Doctype: "Sale", Records: []kernel.RecordMutationBundleItem{{Key: "sale", Doctype: "Sale", Data: data}}}
		for range 2 { // the retry replays the first result
			if cerr := e.runBundle(t, payload, "bundle-sale-b1"); cerr != nil {
				t.Fatalf("bundle: %+v", cerr)
			}
		}
		if got := e.stockOf(t, "SODA"); got != 8 {
			t.Fatalf("stock = %v, want 8", got)
		}
	})
}
