package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

func TestPaymentEndpoints(t *testing.T) {
	type target struct {
		name, driver, dsn string
		d                 kdb.Dialect
		ddl               []string
	}
	targets := []target{{"libsql", "sqlite3", ":memory:", &kdb.LibSQLDialect{}, kdb.PaymentTablesLibSQL()}}
	if v := os.Getenv("KORA_PAYMENT_MYSQL_DSN"); v != "" {
		targets = append(targets, target{"mysql", "mysql", v, &kdb.MySQLDialect{}, kdb.PaymentTablesMySQL()})
	}
	if v := os.Getenv("KORA_PAYMENT_POSTGRES_DSN"); v != "" {
		targets = append(targets, target{"postgres", "postgres", v, &kdb.PostgresDialect{}, kdb.PaymentTablesPostgres()})
	}
	for _, tg := range targets {
		t.Run(tg.name, func(t *testing.T) {
			conn, err := sql.Open(tg.driver, tg.dsn)
			if err != nil {
				t.Fatal(err)
			}
			conn.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = conn.Close() })
			_, _ = conn.Exec(`DROP TABLE IF EXISTS _kora_payment_intent`)
			for _, s := range tg.ddl {
				if _, err := conn.Exec(s); err != nil {
					t.Fatal(err)
				}
			}
			gin.SetMode(gin.TestMode)
			registry := doctype.NewRegistry()
			registry.LoadFull([]*doctype.DocType{{Name: "Order"}}, []*doctype.Role{{Name: "Seller"}, {Name: "Viewer"}},
				[]*doctype.Permission{{Doctype: "Order", Role: "Seller", Create: true}, {Doctype: "Order", Role: "Viewer", Read: true}})
			var active policy.Execution
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set("site_name", "s1")
				c.Set("site_id", "tenant")
				c.Set("site_registry", registry)
				c.Set("site_db", conn)
				c.Set("site_db_type", tg.d.DriverName())
				c.Set("policy_execution", active)
			})
			h := &Handler{Registry: registry}
			r.POST("/payments", h.HandlePaymentRequest)
			r.GET("/payments/:id", h.HandlePaymentGet)
			exec := func(role string) policy.Execution {
				a := contract.ActorContext{PrincipalID: "u", PrincipalType: contract.PrincipalHuman, SubjectUserID: "u", Site: "s1", Roles: []string{role}, AuthenticatedAt: time.Now().UTC()}
				e, err := policy.NewVerifiedExecutionWithGrants("tenant", a, nil)
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
				var out map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &out)
				return w.Code, out
			}
			body := `{"provider":"cash","amount":"12.50","currency":"KES","contact":"c","source_doctype":"Order"}`
			code, out := do("Seller", "POST", "/payments", "k1", body)
			data, _ := out["data"].(map[string]any)
			if code != http.StatusOK || data["state"] != "confirmed" {
				t.Fatalf("request: %d %v", code, out)
			}
			code, out = do("Seller", "POST", "/payments", "k1", body)
			if again, _ := out["data"].(map[string]any); code != 200 || again["id"] != data["id"] {
				t.Fatalf("replay: %d %v", code, out)
			}
			if code, _ = do("Seller", "POST", "/payments", "k1", strings.Replace(body, "12.50", "99", 1)); code != http.StatusConflict {
				t.Fatalf("conflict: %d", code)
			}
			if code, _ = do("Seller", "POST", "/payments", "", body); code != http.StatusBadRequest {
				t.Fatalf("missing key: %d", code)
			}
			if code, _ = do("Viewer", "POST", "/payments", "k2", body); code != http.StatusForbidden {
				t.Fatalf("no permission: %d", code)
			}
			if code, _ = do("Seller", "POST", "/payments", "k3", strings.Replace(body, "cash", "nope", 1)); code != http.StatusBadRequest {
				t.Fatalf("unknown provider: %d", code)
			}
			code, out = do("Seller", "GET", "/payments/"+data["id"].(string), "", "")
			if got, _ := out["data"].(map[string]any); code != 200 || got["id"] != data["id"] {
				t.Fatalf("get: %d %v", code, out)
			}
			if code, _ = do("Seller", "GET", "/payments/missing", "", ""); code != http.StatusNotFound {
				t.Fatalf("get missing: %d", code)
			}
		})
	}
}

func TestFlagSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want bool
	}{
		{"nil", nil, false}, {"true", true, true}, {"false", false, false},
		{"int64 one", int64(1), true}, {"int64 zero", int64(0), false}, {"float", 1.0, true},
		{"bytes one", []byte("1"), true}, {"bytes zero", []byte("0"), false},
		{"text true", "true", true}, {"text empty", "", false},
	} {
		if got := flagSet(tc.in); got != tc.want {
			t.Errorf("%s: flagSet(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}
