package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

func TestPaymentIntentAuthorizedByPublishedOperationGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	for _, s := range kdb.PaymentTablesLibSQL() {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	const site, owner = "pay-op-tenant", contract.ModuleID("shop.core")
	in, out := contract.SchemaRef{ID: "shop.in", Version: 1}, contract.SchemaRef{ID: "shop.out", Version: 1}
	publish := func(name string, payment *contract.PaymentOperationContract) contract.PublishedOperation {
		return contract.PublishedOperation{Name: name, Capability: contract.CapabilityName("shop." + name), Operation: name, ScopePolicy: contract.ScopeTenantShared, Payment: payment}
	}
	binding := &contract.PaymentOperationContract{SourceDoctype: "Order", LinesField: "payments", ReferenceField: "reference", AmountField: "amount", CurrencyField: "currency"}
	ops := []contract.PublishedOperation{publish("checkout", binding), publish("other_checkout", &contract.PaymentOperationContract{SourceDoctype: "Invoice", LinesField: "payments", ReferenceField: "reference", AmountField: "amount", CurrencyField: "currency"}), publish("restock", nil)}
	offers := make([]contract.CapabilityOffer, 0, len(ops))
	for _, p := range ops {
		offers = append(offers, contract.CapabilityOffer{Name: p.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: p.Operation, Input: in, Output: out, Effect: contract.EffectLocal}}})
	}
	snapshot := contract.ModuleSnapshot{TenantID: site, Revision: 1, Modules: []contract.ModuleDescriptor{{ID: owner, Version: "1.0.0", PublishedOperations: ops, Provides: offers}}}
	ref := func(name string) string {
		for _, p := range ops {
			if p.Name == name {
				return contract.PublishedOperationRef(owner, p.Capability, p.Operation)
			}
		}
		return "shop.core~missing~missing"
	}

	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Order"}, {Name: "Invoice"}}, []*doctype.Role{{Name: "Clerk"}, {Name: "Auditor"}, {Name: "Owner"}},
		[]*doctype.Permission{{Doctype: "Order", Role: "Owner", Create: true}})
	for _, name := range []string{"checkout", "other_checkout", "restock"} {
		registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module." + string(owner) + ".operation." + name, Role: "Clerk", Write: true})
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module." + string(owner) + ".operation.checkout", Role: "Owner", Write: true})

	services := NewSiteRuntimeServices()
	services.Replace(site, SiteRuntimeService{LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil }})
	h := NewHandler(registry, nil)
	h.RuntimeServices = services

	tests := []struct {
		name   string
		role   string
		body   string
		status int
	}{
		{"clerk with the operation grant succeeds", "Clerk", `{"operation_ref":"` + ref("checkout") + `","source_doctype":"Order"}`, http.StatusOK},
		{"clerk without operation_ref is denied", "Clerk", `{"source_doctype":"Order"}`, http.StatusForbidden},
		{"role without the operation grant is denied", "Auditor", `{"operation_ref":"` + ref("checkout") + `","source_doctype":"Order"}`, http.StatusForbidden},
		{"operation bound to another source doctype", "Clerk", `{"operation_ref":"` + ref("other_checkout") + `","source_doctype":"Order"}`, http.StatusBadRequest},
		{"operation without a payment binding", "Clerk", `{"operation_ref":"` + ref("restock") + `","source_doctype":"Order"}`, http.StatusBadRequest},
		{"unknown operation ref", "Clerk", `{"operation_ref":"` + ref("nope") + `","source_doctype":"Order"}`, http.StatusNotFound},
		{"arbitrary doctype is denied", "Clerk", `{"operation_ref":"` + ref("checkout") + `","source_doctype":"Invoice"}`, http.StatusBadRequest},
		{"owner with create and no operation_ref unchanged", "Owner", `{"source_doctype":"Order"}`, http.StatusOK},
		{"owner with create and an operation_ref", "Owner", `{"operation_ref":"` + ref("checkout") + `","source_doctype":"Order"}`, http.StatusOK},
		{"owner with create may still name a bad operation", "Owner", `{"operation_ref":"` + ref("restock") + `","source_doctype":"Order"}`, http.StatusBadRequest},
	}
	var active policy.Execution
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("site_name", site)
		c.Set("site_id", site)
		c.Set("site_registry", registry)
		c.Set("site_db", conn)
		c.Set("site_db_type", (&kdb.LibSQLDialect{}).DriverName())
		c.Set("policy_execution", active)
	})
	r.POST("/payments", h.HandlePaymentRequest)
	r.GET("/payments/:id", h.HandlePaymentGet)
	exec := func(role string) policy.Execution {
		a := contract.ActorContext{PrincipalID: "u-" + role, PrincipalType: contract.PrincipalHuman, SubjectUserID: "u-" + role, Site: site, Roles: []string{role}, AuthenticatedAt: time.Now().UTC()}
		e, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(site), a, nil)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	do := func(role, method, path, key, body string) (int, map[string]any) {
		active = exec(role)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var parsed map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &parsed)
		return w.Code, parsed
	}
	ids := map[string]string{}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(tc.body, `{`, `{"provider":"cash","amount":"10","currency":"KES",`, 1)
			code, out := do(tc.role, "POST", "/payments", "k"+string(rune('a'+i)), body)
			if code != tc.status {
				t.Fatalf("status=%d want %d body=%v", code, tc.status, out)
			}
			if data, ok := out["data"].(map[string]any); ok && code == http.StatusOK {
				ids[tc.name], _ = data["id"].(string)
			}
		})
	}
	t.Run("get follows the same grant", func(t *testing.T) {
		id := ids["clerk with the operation grant succeeds"]
		if code, _ := do("Clerk", "GET", "/payments/"+id, "", ""); code != http.StatusOK {
			t.Fatalf("clerk get: %d", code)
		}
		if code, _ := do("Auditor", "GET", "/payments/"+id, "", ""); code != http.StatusForbidden {
			t.Fatalf("auditor get: %d", code)
		}
		if code, _ := do("Owner", "GET", "/payments/"+id, "", ""); code != http.StatusOK {
			t.Fatalf("owner get: %d", code)
		}
	})
}
