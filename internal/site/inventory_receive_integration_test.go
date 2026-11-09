//go:build integration

package site

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
)

// TestInventoryReceiveStockCreatesFirstBalance proves a new item can be
// stocked through the published operation with no pre-existing Stock Balance,
// and that adjust_stock still refuses a missing balance.
func TestInventoryReceiveStockCreatesFirstBalance(t *testing.T) {
	dsn := os.Getenv("KORA_POS_E2E_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_POS_E2E_MYSQL_DSN to a disposable mysql server")
	}
	conn, dbName := posDB(t, "mysql", dsn)
	dialect := kdb.Resolve("mysql")
	if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "inventory-receive.example.test"
	if err := ImportConfig(conn, reg, dbName, siteName, filepath.Join("..", "..", "config", "inventory"), dialect); err != nil {
		t.Fatal(err)
	}
	cmds := kernel.NewCommandRegistry()
	defs, err := kernel.ParseCommandDir(filepath.Join("..", "..", "config", "inventory", "commands"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		d.InputSchema = contract.SchemaRef{ID: "inventory.test." + d.Name + ".in", Version: 1}
		d.OutputSchema = contract.SchemaRef{ID: "inventory.test." + d.Name + ".out", Version: 1}
		if err := cmds.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = cmds
	k.CommandSchemas = allowAllPOSSchemas{}
	actor := contract.ActorContext{PrincipalID: "owner-1", PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now()}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(command, key string, payload any) (contract.CommandResult, *contract.Error) {
		raw, _ := json.Marshal(payload)
		oc := kernel.OperationContext{
			Site: siteName, User: "owner-1", Roles: []string{doctype.AdminRole}, Actor: actor, IdempotencyKey: key,
			Execution: execution, TenantID: contract.TenantID(siteName), Source: kernel.SourceIntegration,
			BusinessScope: contract.BusinessScope{TenantID: contract.TenantID(siteName), Kind: contract.ScopeKindTenant},
		}
		return k.Execute(context.Background(), conn, reg, kernel.Operation{Command: command, Payload: raw, Context: oc})
	}
	create := func(doc string, data map[string]any) string {
		raw, _ := json.Marshal(data)
		res, cerr := exec(kernel.CommandRecordCreate, "", kernel.RecordCreatePayload{Doctype: doc, Data: raw})
		if cerr != nil || res.Status != contract.StatusCompleted {
			t.Fatalf("create %s: %s %+v", doc, res.Status, cerr)
		}
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return fmt.Sprint(out["name"])
	}
	item := create("Item", map[string]any{"item_name": "Tea", "uom": "Nos", "price": 19.99})
	loc := create("Location", map[string]any{"location_name": "Main"})
	command := func(name, key string, qty int) (contract.CommandResult, *contract.Error) {
		return exec(name, key, map[string]any{"data": map[string]any{"item": item, "location": loc, "quantity": qty, "reason": "count"}})
	}
	balance := func() (n int, quantity float64) {
		table := dialect.QuoteIdent(reg.Get("Stock Balance").RawTableName())
		if err := conn.QueryRow("SELECT COUNT(*), COALESCE(MAX(quantity), 0) FROM "+table+" WHERE item = ?", item).Scan(&n, &quantity); err != nil {
			t.Fatal(err)
		}
		return n, quantity
	}

	res, cerr := command("inventory.adjust_stock", "adj-missing", 1)
	if cerr == nil {
		cerr = res.Error
	}
	if res.Status == contract.StatusCompleted || cerr == nil || cerr.Type != contract.CodeNotFound {
		t.Fatalf("adjust on a missing balance: status=%s err=%+v", res.Status, cerr)
	}
	if n, _ := balance(); n != 0 {
		t.Fatalf("adjust_stock created %d balances", n)
	}
	steps := []struct {
		key  string
		qty  int
		want float64
	}{{"recv-1", 50, 50}, {"recv-2", 7, 57}, {"recv-1", 50, 57}}
	for _, st := range steps {
		if res, cerr := command("inventory.receive_stock", st.key, st.qty); cerr != nil || res.Status != contract.StatusCompleted {
			t.Fatalf("receive %s: %s %+v", st.key, res.Status, cerr)
		}
		if n, got := balance(); n != 1 || got != st.want {
			t.Fatalf("after %s: balances=%d quantity=%v, want 1 and %v", st.key, n, got, st.want)
		}
	}
}
