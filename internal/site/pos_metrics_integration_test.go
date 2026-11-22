//go:build integration

package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// The POS metric definitions survive activation and a read back from the stored config version.
func TestPOSMetricsRoundTripAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_POS_E2E_MYSQL_DSN"},
		{"postgres", "KORA_POS_E2E_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			conn, name := posDB(t, tc.driver, dsn)
			dialect := kdb.Resolve(tc.driver)
			if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatal(err)
			}
			const site = "metrics.example.test"
			var dts []*doctype.DocType // POS requires the inventory pack
			for _, pack := range []string{"inventory", "pos"} {
				parsed, err := doctype.ParseConfigTree(filepath.Join("..", "..", "config", pack))
				if err != nil {
					t.Fatal(err)
				}
				dts = append(dts, parsed...)
			}
			raw, _, err := metrics.LoadPack(filepath.Join("..", "..", "config", "pos"), dts)
			if err != nil {
				t.Fatal(err)
			}
			snap := &doctype.ConfigSnapshot{DocTypes: dts, Metrics: raw}
			if err = ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "metrics round trip", snap, dialect, nil); err != nil {
				t.Fatal(err)
			}
			active, _, err := configstore.NewStore(conn, dialect).LoadActiveSnapshot(site)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, r := range active.Metrics {
				var m metrics.Metric
				if err := json.Unmarshal(r, &m); err != nil {
					t.Fatal(err)
				}
				got[m.Name] = true
			}
			for _, want := range []string{"sales", "receipts", "average_sale", "refunds"} {
				if !got[want] {
					t.Fatalf("active config lacks metric %q; got %v", want, got)
				}
			}
		})
	}
}

// posFactsLane admits every maintenance run; the test has no competing workers.
type posFactsLane struct{}

func (posFactsLane) AcquireMaintenance(context.Context) (func(), error) { return func() {}, nil }

// A sale rung up by a cashier through record_sale is grouped by its owner as the cashier's full name, not
// the login. The fact rows come from the Sale events the commands wrote to the outbox, consumed as the
// fact consumer does, so the owner is the one the command stored on the record.
func TestPOSSalesByOwnerLabelFullNameAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_POS_E2E_MYSQL_DSN"},
		{"postgres", "KORA_POS_E2E_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			conn, name := posDB(t, tc.driver, dsn)
			ctx := context.Background()
			dialect := kdb.Resolve(tc.driver)
			if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
				t.Fatal(err)
			}
			const siteName = "pos-owner-label.example.test"
			reg := doctype.NewRegistry()
			if err := ImportConfig(conn, reg, name, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
				t.Fatal(err)
			}
			const login, fullName = "wanjiru@example.test", "Wanjiru Kamau"
			if _, err := conn.ExecContext(ctx, kdb.Rebind(dialect,
				"INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled) VALUES (?, ?, ?, ?, ?, ?)"),
				login, siteName, login, "x", fullName, 1); err != nil {
				t.Fatal(err)
			}
			cmds := kernel.NewCommandRegistry()
			for _, pack := range []string{"inventory", "pos"} {
				defs, err := kernel.ParseCommandDir(filepath.Join("..", "..", "config", pack, "commands"))
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range defs {
					d.InputSchema = contract.SchemaRef{ID: "pos.label." + d.Name + ".in", Version: 1}
					d.OutputSchema = contract.SchemaRef{ID: "pos.label." + d.Name + ".out", Version: 1}
					if err := cmds.Register(d); err != nil {
						t.Fatal(err)
					}
				}
			}
			k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
			k.Commands = cmds
			k.CommandSchemas = allowAllPOSSchemas{}
			scope := contract.BusinessScope{TenantID: contract.TenantID(siteName), Kind: contract.ScopeKindTenant}
			run := func(user string, roles []string, command string, payload any) (map[string]any, *contract.Error) {
				t.Helper()
				actor := contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: roles, AuthenticatedAt: time.Now()}
				execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
				if err != nil {
					t.Fatal(err)
				}
				oc := kernel.OperationContext{Site: siteName, User: user, UserRole: roles[0], Roles: roles, Actor: actor,
					Execution: execution, TenantID: contract.TenantID(siteName), BusinessScope: scope, Source: kernel.SourceIntegration}
				me, merr := contract.NewCommandModuleExecution(scope, "pos.core", "pos.core", 1, command, command)
				if merr != nil {
					t.Fatal(merr)
				}
				oc.ModuleExecution = &me
				raw, _ := json.Marshal(payload)
				res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: oc})
				var out map[string]any
				_ = json.Unmarshal(res.Data, &out)
				return out, cerr
			}
			admin := func(command string, payload any) map[string]any {
				t.Helper()
				out, cerr := run("Administrator", posAdminRoles, command, payload)
				if cerr != nil {
					t.Fatalf("%s: %+v", command, cerr)
				}
				return out
			}
			create := func(doc string, data map[string]any) string {
				raw, _ := json.Marshal(data)
				return fmt.Sprint(admin(kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: doc, Data: raw})["name"])
			}
			loc := create("Location", map[string]any{"location_name": "Main"})
			tea := create("Item", map[string]any{"item_name": "Tea", "uom": "Nos", "price": 100})
			create("Stock Balance", map[string]any{"item": tea, "location": loc, "quantity": 500, "reorder_level": 1})
			register := create("Register", map[string]any{"register_name": "R1", "location": loc, "enabled": 1})

			cashier := []string{"Cashier"}
			out, cerr := run(login, cashier, "pos.open_session", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 100}})
			if cerr != nil {
				t.Fatalf("open_session: %+v", cerr)
			}
			till := fmt.Sprint(out["output"].(map[string]any)["reference"])
			pay := posPayments(t, conn, dialect, siteName)
			if _, cerr := run(login, cashier, "pos.record_sale", map[string]any{"data": map[string]any{
				"register": register, "till_session": till, "location": loc, "currency": "KES",
				"items":    []map[string]any{{"item": tea, "quantity": 3, "unit_price": 100, "amount": 300}},
				"payments": []map[string]any{pay("cash", 300)}}}); cerr != nil {
				t.Fatalf("record_sale: %+v", cerr)
			}

			// Feed the Sale events the commands wrote through the fact consumer.
			sale := reg.Get("Sale")
			if sale == nil || sale.Facts == nil {
				t.Fatal("pos pack has no Sale fact model")
			}
			factReg := doctype.NewRegistry()
			factReg.Register(sale)
			feedSaleEvents(t, conn, dialect, factReg)
			refresher := &facts.Refresher{DB: conn, Dialect: dialect, Registry: factReg, Admission: posFactsLane{}, Owner: "pos-owner-label"}
			if _, err := refresher.RefreshDue(ctx); err != nil {
				t.Fatalf("refresh: %v", err)
			}

			defs, err := metrics.ParseDirectory(filepath.Join("..", "..", "config", "pos", "metrics"))
			if err != nil {
				t.Fatal(err)
			}
			engine := metrics.Engine{DB: conn, Dialect: dialect, Site: siteName, Catalog: metrics.NewCatalog(defs, reg.All())}
			// The sale is dated today in the tenant zone; the window covers a day either side of today in UTC.
			now := time.Now().UTC()
			from, to := now.AddDate(0, 0, -1), now.AddDate(0, 0, 1)
			res, qerr := engine.Query(ctx, metrics.Query{Metrics: []string{"sales"}, Dimensions: []string{"owner"}, Grain: metrics.GrainDay,
				From: tenanttime.Date{Year: from.Year(), Month: from.Month(), Day: from.Day()},
				To:   tenanttime.Date{Year: to.Year(), Month: to.Month(), Day: to.Day()}})
			if qerr != nil {
				t.Fatalf("sales by owner: %v", qerr)
			}
			var total decimal.Decimal
			for _, row := range res.Rows {
				if len(row.Group) != 1 || row.Group[0] != login {
					t.Fatalf("group = %q, want the login %q", row.Group, login)
				}
				if len(row.Labels) != 1 || row.Labels[0] != fullName {
					t.Fatalf("labels = %q, want the cashier's full name %q", row.Labels, fullName)
				}
				if v := row.Values[0]; v.Valid {
					total = total.Add(v.Decimal)
				}
			}
			if !total.Equal(decimal.NewFromInt(300)) {
				t.Fatalf("sales total = %s over %d rows, want 300", total, len(res.Rows))
			}
		})
	}
}

// feedSaleEvents consumes every Sale insert in the outbox through the fact builder, in source order.
func feedSaleEvents(t *testing.T, conn *sql.DB, dialect kdb.Dialect, reg *doctype.Registry) {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.QueryContext(ctx, kdb.Rebind(dialect,
		"SELECT payload, source_sequence FROM _kora_outbox WHERE event_type = ? ORDER BY source_sequence"),
		outbox.EventTypeName("Sale", eventstream.EventInsert))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	type stored struct {
		payload string
		seq     int64
	}
	var events []stored
	for rows.Next() {
		var s stored
		if err := rows.Scan(&s.payload, &s.seq); err != nil {
			t.Fatal(err)
		}
		events = append(events, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("record_sale wrote no Sale event")
	}
	builder := &facts.Builder{DB: conn, Dialect: dialect, Registry: reg}
	for _, s := range events {
		var ev contract.EventEnvelope
		if err := json.Unmarshal([]byte(s.payload), &ev); err != nil {
			t.Fatal(err)
		}
		ev.Sequence = contract.Revision(fmt.Sprint(s.seq))
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := builder.Consume(ctx, tx, ev); err != nil {
			_ = tx.Rollback()
			t.Fatalf("consume %s: %v", ev.ID, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}
