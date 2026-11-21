//go:build integration

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// TestSavedReportsOnDisposableDatabases runs the saved report routes on MySQL and PostgreSQL: the store round
// trip, the administrator gate on shop reports, the owner gate on private ones, the name rules, copies, and the
// broken state set when a configuration drops a field a report uses.
func TestSavedReportsOnDisposableDatabases(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_SAVED_REPORT_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_SAVED_REPORT_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			database := disposabledb.Open(t, tc.dialect, dsn)
			kernel := kdb.KernelTablesMySQL()
			if tc.dialect == "postgres" {
				kernel = kdb.KernelTablesPostgres()
			}
			for _, ddl := range append(createTableStatements(kdb.Resolve(tc.dialect).SystemTableSQL()), kernel...) {
				if _, err := database.DB.Exec(ddl); err != nil {
					t.Fatalf("create system tables: %v", err)
				}
			}
			registry := reportRegistry(true)
			router := reportServer(t, database.DB, tc.dialect, registry)
			exerciseReports(t, router, database.DB, tc.dialect)
		})
	}
}

// reportRegistry has Expense with a fact model, readable by Cashier, and Payroll, readable by nobody but the
// administrator. withAmount=false drops the amount measure, as a pack update would.
func reportRegistry(withAmount bool) *doctype.Registry {
	measures := []doctype.FactMeasure{{Field: "amount"}}
	if !withAmount {
		measures = nil
	}
	expense := &doctype.DocType{Name: "Expense", Facts: &doctype.FactModel{Time: "creation", Measures: measures,
		Dimensions: []doctype.FactDimension{{Field: "category"}, {Field: "supplier"}}},
		Fields: []doctype.Field{{Fieldname: "supplier", Fieldtype: "Link", Options: "Supplier"}}}
	payroll := &doctype.DocType{Name: "Payroll", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "net"}}}}
	supplier := &doctype.DocType{Name: "Supplier", Fields: []doctype.Field{{Fieldname: "town", Fieldtype: "Data"}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{expense, payroll, supplier}, []*doctype.Role{{Name: "Cashier"}}, []*doctype.Permission{
		{Doctype: "Expense", Role: "Cashier", Read: true},
		{Doctype: "Expense", Role: doctype.AdminRole, Read: true},
		{Doctype: "Payroll", Role: doctype.AdminRole, Read: true},
		{Doctype: "Supplier", Role: doctype.AdminRole, Read: true},
	})
	return registry
}

// reportServer mounts the real metric routes. The caller is named by the X-Roles and X-User headers.
func reportServer(t *testing.T, database *sql.DB, dialect string, registry *doctype.Registry) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		user := c.GetHeader("X-User")
		actor := contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, SubjectUserID: user,
			Site: "tenant-reports", Roles: strings.Split(c.GetHeader("X-Roles"), ","), AuthenticatedAt: time.Now().UTC()}
		execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID("stable-tenant"), actor, nil)
		if err != nil {
			t.Fatal("create verified execution:", err)
		}
		c.Set("site_name", "tenant-reports")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", registry)
		c.Set("policy_execution", execution)
		c.Next()
	})
	registerMetricsRoutes(router.Group("/api/v1"), registry, database, kdb.Resolve(dialect))
	return router
}

// reportCall sends one request as a user with a role and checks the status.
func reportCall(t *testing.T, router *gin.Engine, role, user, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Roles", role)
	req.Header.Set("X-User", user)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != want {
		t.Fatalf("%s %s as %s/%s = %d, want %d: %s", method, path, role, user, recorder.Code, want, recorder.Body)
	}
	return recorder
}

// savedReportBody returns the JSON body that saves a report.
func savedReportBody(t *testing.T, name, visibility string, q contract.SavedQuestion) string {
	t.Helper()
	raw, err := json.Marshal(contract.SavedReportRequest{Name: name, Visibility: visibility, Question: q})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// reportNames lists the reports a caller sees, by name.
func reportNames(t *testing.T, router *gin.Engine, role, user string) map[string]contract.SavedReport {
	t.Helper()
	recorder := reportCall(t, router, role, user, http.MethodGet, "/api/v1/explore/reports", "", http.StatusOK)
	var envelope struct {
		Data []contract.SavedReport `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode reports:", err)
	}
	out := map[string]contract.SavedReport{}
	for _, rep := range envelope.Data {
		out[rep.Name] = rep
	}
	return out
}

// reportID returns the id of the report a caller sees under name.
func reportID(t *testing.T, router *gin.Engine, role, user, name string) string {
	t.Helper()
	rep, ok := reportNames(t, router, role, user)[name]
	if !ok {
		t.Fatalf("report %q not visible to %s", name, user)
	}
	return rep.ID
}

// exerciseReports walks the report rules in order on one database.
func exerciseReports(t *testing.T, router *gin.Engine, database *sql.DB, dialect string) {
	t.Helper()
	const base = "/api/v1/explore/reports"
	count := contract.SavedQuestion{RecordType: "Expense", Measure: "sum:amount", By: []string{"category"}, Grain: "month", Top: 5,
		Filters: []contract.MetricFilter{{Field: "category", Op: "=", Value: []byte(`"fuel"`)}}}
	payroll := contract.SavedQuestion{RecordType: "Payroll", Measure: "count"}

	// Only the administrator saves shop reports; a cashier saves a private one, and the store keeps the question.
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPost, base, savedReportBody(t, "Fuel by month", "shop", count), http.StatusForbidden)
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPost, base, savedReportBody(t, "My fuel", "private", count), http.StatusCreated)
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPost, base, savedReportBody(t, "  my FUEL ", "private", count), http.StatusConflict)
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPost, base, savedReportBody(t, "   ", "private", count), http.StatusUnprocessableEntity)
	raceCreates(t, router, base, savedReportBody(t, "Race fuel", "private", count))
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPost, base, savedReportBody(t, "Ghost", "private", contract.SavedQuestion{RecordType: "Ghost"}), http.StatusUnprocessableEntity)
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPost, base, savedReportBody(t, "Payroll peek", "private", payroll), http.StatusForbidden)
	reportCall(t, router, doctype.AdminRole, "admin-1", http.MethodPost, base, savedReportBody(t, "Payroll net", "shop", payroll), http.StatusCreated)
	reportCall(t, router, doctype.AdminRole, "admin-1", http.MethodPost, base, savedReportBody(t, "Fuel by month", "shop", count), http.StatusCreated)

	// A question through a Link lists the linked record type too: a cashier who cannot read Supplier can neither
	// save the path nor see the shop report that uses it.
	towns := contract.SavedQuestion{RecordType: "Expense", By: []string{"supplier.town"}}
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPost, base, savedReportBody(t, "Towns", "private", towns), http.StatusUnprocessableEntity)
	reportCall(t, router, doctype.AdminRole, "admin-1", http.MethodPost, base, savedReportBody(t, "Towns", "shop", towns), http.StatusCreated)
	if _, ok := reportNames(t, router, "Cashier", "cashier-1")["Towns"]; ok {
		t.Fatal("cashier sees a report that reads Supplier")
	}
	if _, ok := reportNames(t, router, doctype.AdminRole, "admin-1")["Towns"]; !ok {
		t.Fatal("admin does not see the Supplier report")
	}

	mine := reportID(t, router, "Cashier", "cashier-1", "My fuel")
	if got := reportNames(t, router, "Cashier", "cashier-1"); got["Payroll net"].Name != "" || got["Fuel by month"].Visibility != "shop" {
		t.Fatalf("cashier sees %v, want shop Fuel by month and no Payroll report", got)
	}
	if names := reportNames(t, router, "Cashier", "cashier-2"); names["My fuel"].Name != "" {
		t.Fatal("another user sees a private report")
	}
	reportCall(t, router, "Cashier", "cashier-2", http.MethodGet, base+"/"+mine, "", http.StatusNotFound)

	var envelope struct {
		Data contract.SavedReport `json:"data"`
	}
	if err := json.Unmarshal(reportCall(t, router, "Cashier", "cashier-1", http.MethodGet, base+"/"+mine, "", http.StatusOK).Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	stored := envelope.Data
	if stored.Owner != "cashier-1" || stored.Question.Measure != "sum:amount" || len(stored.Question.By) != 1 || stored.Question.Top != 5 || len(stored.Question.Filters) != 1 {
		t.Fatalf("stored question = %+v", stored)
	}

	// The owner renames a private report; the administrator cannot change someone else's private one.
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPut, base+"/"+mine, savedReportBody(t, "My fuel by month", "private", count), http.StatusOK)
	reportCall(t, router, "Cashier", "cashier-2", http.MethodPut, base+"/"+mine, savedReportBody(t, "Stolen", "private", count), http.StatusNotFound)
	reportCall(t, router, doctype.AdminRole, "admin-1", http.MethodDelete, base+"/"+mine, "", http.StatusNotFound)

	// A copy of a shop report belongs to the caller and can be private.
	shop := reportID(t, router, "Cashier", "cashier-1", "Fuel by month")
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPost, base+"/"+shop+"/copy", `{"name":"Fuel copy","visibility":"private"}`, http.StatusCreated)
	reportCall(t, router, "Cashier", "cashier-1", http.MethodPut, base+"/"+shop, savedReportBody(t, "Fuel by month", "shop", count), http.StatusForbidden)
	reportCall(t, router, "Cashier", "cashier-1", http.MethodDelete, base+"/"+shop, "", http.StatusForbidden)

	// A pack update drops the amount measure. The activation check marks the report broken, names the
	// field, keeps it listed, and an update to a valid question clears the state.
	markBrokenReports(context.Background(), database, kdb.Resolve(dialect), &doctype.ConfigSnapshot{}, reportRegistry(false))
	broken := reportNames(t, router, "Cashier", "cashier-1")["Fuel by month"]
	if broken.BrokenReason == "" || broken.BrokenAt == "" || !strings.Contains(broken.BrokenReason, "sum:amount") {
		t.Fatalf("broken report = %+v, want a reason naming sum:amount and a time", broken)
	}
	reportCall(t, router, doctype.AdminRole, "admin-1", http.MethodPut, base+"/"+shop, savedReportBody(t, "Fuel by month", "shop", contract.SavedQuestion{RecordType: "Expense"}), http.StatusOK)
	if fixed := reportNames(t, router, "Cashier", "cashier-1")["Fuel by month"]; fixed.BrokenReason != "" || fixed.BrokenAt != "" {
		t.Fatalf("fixed report still broken: %+v", fixed)
	}

	reportCall(t, router, doctype.AdminRole, "admin-1", http.MethodDelete, base+"/"+shop, "", http.StatusNoContent)
	reportCall(t, router, "Cashier", "cashier-1", http.MethodDelete, base+"/"+mine, "", http.StatusNoContent)
	reportCall(t, router, "Cashier", "cashier-1", http.MethodGet, base+"/"+mine, "", http.StatusNotFound)
}

// raceCreates sends two creates of the same name at once. The name check passes for both, so the unique index
// decides: one is saved and one is refused with 409.
func raceCreates(t *testing.T, router *gin.Engine, path, body string) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("X-Roles", "Cashier")
			req.Header.Set("X-User", "cashier-race")
			recorder := httptest.NewRecorder()
			<-start
			router.ServeHTTP(recorder, req)
			codes <- recorder.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	var created, refused int
	for code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			refused++
		default:
			t.Fatalf("concurrent create = %d, want 201 or 409", code)
		}
	}
	if created != 1 || refused != 1 {
		t.Fatalf("concurrent creates: %d created, %d refused; want 1 and 1", created, refused)
	}
}
