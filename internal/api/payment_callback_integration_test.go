//go:build integration

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/payment"
	"github.com/asenawritescode/kora/internal/payment/mpesa"
)

type cbSecrets map[string]string

func (f cbSecrets) Get(_, k string) (string, error) {
	if v, ok := f[k]; ok {
		return v, nil
	}
	return "", errors.New("missing")
}

func TestPaymentCallbackRoute(t *testing.T) {
	for _, tg := range []struct {
		name, driver, env string
		d                 kdb.Dialect
		ddl               []string
	}{
		{"mysql", "mysql", "KORA_PAYMENT_MYSQL_DSN", &kdb.MySQLDialect{}, kdb.PaymentTablesMySQL()},
		{"postgres", "postgres", "KORA_PAYMENT_POSTGRES_DSN", &kdb.PostgresDialect{}, kdb.PaymentTablesPostgres()},
	} {
		t.Run(tg.name, func(t *testing.T) {
			dsn := os.Getenv(tg.env)
			if dsn == "" {
				t.Skipf("set %s", tg.env)
			}
			conn, err := sql.Open(tg.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			_, _ = conn.Exec(`DROP TABLE IF EXISTS _kora_payment_intent`)
			for _, s := range tg.ddl {
				if _, err := conn.Exec(s); err != nil {
					t.Fatal(err)
				}
			}
			n := 0
			daraja := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/oauth") {
					_, _ = w.Write([]byte(`{"access_token":"t","expires_in":"3599"}`))
					return
				}
				n++
				_ = json.NewEncoder(w).Encode(map[string]string{"ResponseCode": "0", "CheckoutRequestID": "ws_CO_" + string(rune('A'+n))})
			}))
			defer daraja.Close()
			mp := mpesa.New(mpesa.Config{Site: "s1", BaseURL: daraja.URL, Client: daraja.Client(), Secrets: cbSecrets{
				"mpesa_consumer_key": "ck", "mpesa_consumer_secret": "cs", "mpesa_shortcode": "174379",
				"mpesa_passkey": "pk", "mpesa_callback_token": "tok"}})
			svc := &payment.Service{Store: &payment.SQLStore{DB: conn, Dialect: tg.d}, Providers: payment.NewRegistry()}
			_ = svc.Providers.Register(mp)
			old := PaymentServiceFactory
			PaymentServiceFactory = func(*sql.DB, kdb.Dialect) *payment.Service { return svc }
			t.Cleanup(func() { PaymentServiceFactory = old })

			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set("site_name", "s1")
				c.Set("site_db", conn)
				c.Set("site_db_type", tg.d.DriverName())
			})
			r.POST("/webhooks/payments/:provider/:token", (&Handler{}).HandlePaymentCallback)
			post := func(token, body string) int {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest("POST", "/webhooks/payments/mpesa/"+token, strings.NewReader(body)))
				return w.Code
			}
			cb := func(id string, code int) string {
				return `{"Body":{"stkCallback":{"CheckoutRequestID":"` + id + `","ResultCode":` + strconv.Itoa(code) + `,"ResultDesc":"desc",` +
					`"CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"RCPT1"}]}}}}`
			}
			ctx := context.Background()
			req := func(key string) payment.Intent {
				it, err := svc.Request(ctx, "s1", "u", payment.RequestInput{Provider: "mpesa", Amount: decimal.NewFromInt(10), Currency: "KES", Contact: "254700000000", IdempotencyKey: key})
				if err != nil || it.State != payment.StatePending {
					t.Fatalf("request: %v %+v", err, it)
				}
				return it
			}
			ok, bad := req("k1"), req("k2")
			if c := post("wrong", cb(ok.ProviderRequestID, 0)); c != 401 {
				t.Fatalf("bad token: %d", c)
			}
			if c := post("tok", cb(ok.ProviderRequestID, 0)); c != 200 {
				t.Fatalf("success: %d", c)
			}
			got, _ := svc.Get(ctx, "s1", ok.ID)
			if got.State != payment.StateConfirmed || got.ProviderReference != "RCPT1" {
				t.Fatalf("not confirmed: %+v", got)
			}
			if c := post("tok", cb(ok.ProviderRequestID, 0)); c != 200 {
				t.Fatalf("replay: %d", c)
			}
			if c := post("tok", cb(ok.ProviderRequestID, 1032)); c != 200 {
				t.Fatalf("conflict: %d", c)
			}
			if again, _ := svc.Get(ctx, "s1", ok.ID); again.State != payment.StateConfirmed || again.UpdatedAt != got.UpdatedAt {
				t.Fatalf("replay/conflict wrote: %+v", again)
			}
			if c := post("tok", cb("ws_CO_unknown", 0)); c != 404 {
				t.Fatalf("unknown: %d", c)
			}
			if c := post("tok", cb(bad.ProviderRequestID, 1032)); c != 200 {
				t.Fatalf("failed: %d", c)
			}
			if f, _ := svc.Get(ctx, "s1", bad.ID); f.State != payment.StateFailed || f.FailureReason != "desc" {
				t.Fatalf("not failed: %+v", f)
			}
		})
	}
}
