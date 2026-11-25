//go:build integration

package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// TestRecordToolsReachPackTypesAcrossDialects runs a scripted sequence of tool calls, as the
// chat loop would, against the real inventory and POS packs. It creates an Item, finds a Sale,
// and checks that a Sale write is refused with its pack command named.
func TestRecordToolsReachPackTypesAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
	}{
		{name: "mysql", dsn: "KORA_AI_RECORD_TOOLS_MYSQL_DSN"},
		{name: "postgres", dsn: "KORA_AI_RECORD_TOOLS_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.dsn)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.dsn, tc.name)
			}
			database, err := sql.Open(tc.name, dsn)
			if err != nil {
				t.Fatalf("open %s database: %v", tc.name, err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if err = database.Ping(); err != nil {
				t.Fatalf("ping %s database: %v", tc.name, err)
			}
			dialect := kdb.Resolve(tc.name)
			ctx := WithDialect(context.Background(), dialect)
			if err = ksite.BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatalf("bootstrap %s system tables: %v", tc.name, err)
			}
			registry := loadPackRegistry(t, "inventory", "pos")
			for _, dt := range registry.All() {
				for _, statement := range dialect.CreateTable(dt) {
					if _, err = database.Exec(statement); err != nil {
						t.Fatalf("create %s table %s: %v", tc.name, dt.Name, err)
					}
				}
			}
			commands, err := kernel.LoadCommandDir(filepath.Join("..", "..", "..", "config", "pos", "commands"))
			if err != nil {
				t.Fatalf("load POS commands: %v", err)
			}

			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			site := "ai-record-tools-" + tc.name + "-" + suffix
			installActiveModules(t, database, dialect, site, "inventory.core", "pos.core")
			execution := testAIExecution(t, site, "alice@example.test", []string{doctype.AdminRole})
			tx := &orm.TxManager{
				DB: database, Registry: registry, Dialect: dialect, SiteName: site,
				Context: ctx, CurrentUser: "alice@example.test", CurrentUserRole: doctype.AdminRole,
				CurrentUserRoles: []string{doctype.AdminRole}, Outbox: outbox.NewSQLWriter(dialect), Execution: execution,
			}
			tx.Context = WithDialect(tx.Context, dialect)

			// A Sale exists before the chat, as one written by the POS. The chat must find it.
			dbTx, err := database.Begin()
			if err != nil {
				t.Fatalf("begin sale insert: %v", err)
			}
			register := doctype.NewDocument("Register")
			register.Fields = map[string]any{"register_name": "Till 1", "enabled": 1}
			if err = tx.InsertInTx(dbTx, registry.Get("Register"), register, "alice@example.test", "alice@example.test"); err != nil {
				_ = dbTx.Rollback()
				t.Fatalf("insert Register on %s: %v", tc.name, err)
			}
			sale := doctype.NewDocument("Sale")
			sale.Fields = map[string]any{"register": register.Name, "status": "Paid", "total": 250}
			if err = tx.InsertInTx(dbTx, registry.Get("Sale"), sale, "alice@example.test", "alice@example.test"); err != nil {
				_ = dbTx.Rollback()
				t.Fatalf("insert Sale on %s: %v", tc.name, err)
			}
			if err = dbTx.Commit(); err != nil {
				t.Fatalf("commit Sale on %s: %v", tc.name, err)
			}

			// runInTurn runs one chat turn: its run ID is the turn, so a preview cannot confirm itself.
			runInTurn := func(runID, step string, calls ...map[string]any) []string {
				anyCalls := make([]any, 0, len(calls))
				for _, call := range calls {
					anyCalls = append(anyCalls, call)
				}
				results := RunToolTurn(tx.Context, tx, registry, toolCallBatch{
					Calls: anyCalls, Owner: "alice@example.test", Site: site, RunID: runID, StepID: step,
					ConversationID: "conversation-" + suffix, Execution: execution, Commands: commands,
				})
				contents := make([]string, 0, len(results))
				for _, result := range results {
					content, _ := result["content"].(string)
					contents = append(contents, content)
				}
				return contents
			}
			run := func(step string, calls ...map[string]any) []string {
				return runInTurn("run-"+suffix, step, calls...)
			}
			toolCall := func(id, name, args string) map[string]any {
				return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
			}

			described := run("step-describe", toolCall("call-describe", toolDescribeRecord, `{"record_type":"Item"}`))[0]
			if !strings.Contains(described, `"item_name"`) {
				t.Fatalf("describe Item on %s = %q, want item_name field", tc.name, described)
			}
			preview := run("step-create", toolCall("call-create", toolCreateRecord, `{"record_type":"Item","data":{"item_name":"Sugar 2kg","uom":"Each","price":250}}`))[0]
			if !strings.HasPrefix(preview, `{"preview"`) {
				t.Fatalf("create Item on %s = %q, want a preview", tc.name, preview)
			}
			token := writeIDOf(t, preview)
			created := runInTurn("run-"+suffix+"-confirm", "step-confirm", toolCall("call-confirm", toolConfirmWrite, `{"write_id":"`+token+`"}`))[0]
			if !strings.HasPrefix(created, "Created Item") {
				t.Fatalf("confirm Item on %s = %q, want Created Item", tc.name, created)
			}
			found := run("step-find-item", toolCall("call-find-item", toolFindRecords, `{"record_type":"Item","search":"Sugar"}`))[0]
			if !strings.Contains(found, "Found 1 Item") {
				t.Fatalf("find Item on %s = %q, want one Item", tc.name, found)
			}
			sales := run("step-find-sale", toolCall("call-find-sale", toolFindRecords, `{"record_type":"Sale"}`))[0]
			if !strings.Contains(sales, "Found 1 Sale") {
				t.Fatalf("find Sale on %s = %q, want one Sale", tc.name, sales)
			}
			refused := run("step-sale-write", toolCall("call-sale-write", toolCreateRecord, `{"record_type":"Sale","data":{"total":10}}`))[0]
			if !strings.Contains(refused, "module_command_required") || !strings.Contains(refused, "pos.record_sale") {
				t.Fatalf("create Sale on %s = %q, want the pos.record_sale command named", tc.name, refused)
			}
		})
	}
}

// loadPackRegistry parses the named config packs from the repository config directory into one registry.
func loadPackRegistry(t *testing.T, packs ...string) *doctype.Registry {
	t.Helper()
	registry := doctype.NewRegistry()
	for _, pack := range packs {
		doctypes, err := doctype.ParseConfigTree(filepath.Join("..", "..", "..", "config", pack))
		if err != nil {
			t.Fatalf("parse %s pack: %v", pack, err)
		}
		for _, dt := range doctypes {
			registry.Register(dt)
		}
	}
	return registry
}

// installActiveModules records the pack modules as active for the tenant, as activation does. Generic
// writes to a module-owned record type are refused until its owning module is installed.
func installActiveModules(t *testing.T, database *sql.DB, dialect kdb.Dialect, site string, modules ...string) {
	t.Helper()
	store := configstore.NewModuleStore(database, dialect)
	if err := store.EnsureSchema(); err != nil {
		t.Fatal("create module lifecycle snapshot table:", err)
	}
	snapshot := contract.ModuleSnapshot{TenantID: contract.TenantID(site), Revision: 1}
	for _, module := range modules {
		snapshot.Modules = append(snapshot.Modules, contract.ModuleDescriptor{ID: contract.ModuleID(module), Version: "1.0.0"})
		snapshot.Installations = append(snapshot.Installations, contract.ModuleInstallation{TenantID: contract.TenantID(site), ModuleID: contract.ModuleID(module), Version: "1.0.0", ArtifactDigest: "fixture", State: string(contract.ModuleActive), Revision: 2})
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal("encode module snapshot:", err)
	}
	if _, err := database.Exec(kdb.Rebind(dialect, `INSERT INTO _kora_module_snapshot (tenant_id,revision,snapshot_json) VALUES (?,?,?)`), site, snapshot.Revision, string(payload)); err != nil {
		t.Fatal("persist module snapshot:", err)
	}
}

// TestUpdatePreviewListsOnlyChangedFieldsAcrossDialects previews an update that changes only a
// Select field of a Stock Entry. Its changes and its summary line must name that one field: the
// Float quantity and the empty remarks are read back unchanged and are not part of the change.
func TestUpdatePreviewListsOnlyChangedFieldsAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ name, dsn string }{
		{name: "mysql", dsn: "KORA_AI_RECORD_TOOLS_MYSQL_DSN"},
		{name: "postgres", dsn: "KORA_AI_RECORD_TOOLS_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.dsn)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.dsn, tc.name)
			}
			database := isolatedAIDatabase(t, tc.name, dsn)
			dialect := kdb.Resolve(tc.name)
			ctx := WithDialect(context.Background(), dialect)
			if err := ksite.BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatalf("bootstrap %s system tables: %v", tc.name, err)
			}
			registry := loadPackRegistry(t, "inventory")
			for _, dt := range registry.All() {
				for _, statement := range dialect.CreateTable(dt) {
					if _, err := database.Exec(statement); err != nil {
						t.Fatalf("create %s table %s: %v", tc.name, dt.Name, err)
					}
				}
			}
			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			site := "ai-update-preview-" + tc.name + "-" + suffix
			installActiveModules(t, database, dialect, site, "inventory.core")
			execution := testAIExecution(t, site, "alice@example.test", []string{doctype.AdminRole})
			tx := &orm.TxManager{
				DB: database, Registry: registry, Dialect: dialect, SiteName: site,
				Context: ctx, CurrentUser: "alice@example.test", CurrentUserRole: doctype.AdminRole,
				CurrentUserRoles: []string{doctype.AdminRole}, Outbox: outbox.NewSQLWriter(dialect), Execution: execution,
			}
			tx.Context = WithDialect(tx.Context, dialect)

			dbTx, err := database.Begin()
			if err != nil {
				t.Fatalf("begin stock insert: %v", err)
			}
			location := doctype.NewDocument("Location")
			location.Fields = map[string]any{"location_name": "Shop floor"}
			item := doctype.NewDocument("Item")
			item.Fields = map[string]any{"item_name": "Sugar 2kg", "uom": "Each", "price": 250}
			entry := doctype.NewDocument("Stock Entry")
			entry.Fields = map[string]any{"quantity": 2.5, "entry_type": "Receipt"}
			for _, step := range []struct {
				dt  string
				doc *doctype.Document
			}{{"Location", location}, {"Item", item}} {
				if err = tx.InsertInTx(dbTx, registry.Get(step.dt), step.doc, "alice@example.test", "alice@example.test"); err != nil {
					_ = dbTx.Rollback()
					t.Fatalf("insert %s on %s: %v", step.dt, tc.name, err)
				}
			}
			entry.Fields["item"], entry.Fields["location"] = item.Name, location.Name
			if err = tx.InsertInTx(dbTx, registry.Get("Stock Entry"), entry, "alice@example.test", "alice@example.test"); err != nil {
				_ = dbTx.Rollback()
				t.Fatalf("insert Stock Entry on %s: %v", tc.name, err)
			}
			if err = dbTx.Commit(); err != nil {
				t.Fatalf("commit Stock Entry on %s: %v", tc.name, err)
			}

			args := `{"record_type":"Stock Entry","name":"` + entry.Name + `","data":{"entry_type":"Issue"}}`
			calls := []any{map[string]any{"id": "call-update", "type": "function", "function": map[string]any{"name": toolUpdateRecord, "arguments": args}}}
			results := RunToolTurn(tx.Context, tx, registry, toolCallBatch{
				Calls: calls, Owner: "alice@example.test", Site: site, RunID: "run-update", StepID: "step-update",
				ConversationID: "conversation-" + suffix, Execution: execution,
			})
			if len(results) != 1 {
				t.Fatalf("update preview on %s returned %d results, want one", tc.name, len(results))
			}
			preview, _ := results[0]["content"].(string)
			var reply struct {
				Preview map[string]json.RawMessage `json:"preview"`
			}
			if err := json.Unmarshal([]byte(preview), &reply); err != nil {
				t.Fatalf("update preview on %s = %q, want a preview: %v", tc.name, preview, err)
			}
			var changes map[string]json.RawMessage
			if err := json.Unmarshal(reply.Preview["changes"], &changes); err != nil {
				t.Fatalf("decode changes on %s: %v", tc.name, err)
			}
			if len(changes) != 1 || changes["entry_type"] == nil {
				t.Fatalf("changes on %s = %s, want only entry_type", tc.name, reply.Preview["changes"])
			}
			summary := pendingWrite{ToolName: toolUpdateRecord, PreviewJSON: preview}.summary()
			want := "update Stock Entry " + entry.Name + " (entry_type Receipt -> Issue)"
			if summary != want {
				t.Fatalf("summary on %s = %q, want %q", tc.name, summary, want)
			}
		})
	}
}
