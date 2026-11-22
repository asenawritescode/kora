//go:build integration

package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// TestChatWritesArePreviewedThenConfirmedAcrossDialects runs scripted chat turns against the
// inventory and POS packs. Each turn has its own run ID, as the chat loop gives it. A create
// previews without a row, a confirm in a later turn writes exactly one row, and the refusals
// of confirm_write are checked: same turn, another person, another site or conversation,
// expired, changed arguments, and a replay that returns the first outcome.
func TestChatWritesArePreviewedThenConfirmedAcrossDialects(t *testing.T) {
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
			registry := loadPackRegistry(t, "inventory", "pos")
			for _, dt := range registry.All() {
				for _, statement := range dialect.CreateTable(dt) {
					if _, err := database.Exec(statement); err != nil {
						t.Fatalf("create %s table %s: %v", tc.name, dt.Name, err)
					}
				}
			}
			commands, err := kernel.LoadCommandDir(filepath.Join("..", "..", "..", "config", "pos", "commands"))
			if err != nil {
				t.Fatalf("load POS commands: %v", err)
			}
			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			site := "ai-pending-" + tc.name + "-" + suffix
			installActiveModules(t, database, dialect, site, "inventory.core", "pos.core")
			alice := testAIExecution(t, site, "alice@example.test", []string{doctype.AdminRole})
			bob := testAIExecution(t, site, "bob@example.test", []string{doctype.AdminRole})
			tx := &orm.TxManager{
				DB: database, Registry: registry, Dialect: dialect, SiteName: site,
				Context: ctx, CurrentUser: "alice@example.test", CurrentUserRole: doctype.AdminRole,
				CurrentUserRoles: []string{doctype.AdminRole}, Outbox: outbox.NewSQLWriter(dialect), Execution: alice,
			}
			tx.Context = WithDialect(tx.Context, dialect)

			conversation := "conversation-" + suffix
			// turn runs one chat turn: one run ID, one conversation, the given caller.
			turn := func(run string, execution policy.Execution, owner, siteName, conv string, calls ...map[string]any) string {
				t.Helper()
				anyCalls := make([]any, 0, len(calls))
				for _, call := range calls {
					anyCalls = append(anyCalls, call)
				}
				results := RunToolTurn(tx.Context, tx, registry, toolCallBatch{
					Calls: anyCalls, Owner: owner, Site: siteName, RunID: run, StepID: "step-" + run,
					ConversationID: conv, Execution: execution, Commands: commands,
				})
				content, _ := results[len(results)-1]["content"].(string)
				return content
			}
			call := func(id, name, args string) map[string]any {
				return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
			}
			count := func(doctype, column, value string) int {
				t.Helper()
				var n int
				query := "SELECT COUNT(*) FROM " + dialect.QuoteIdent(registry.Get(doctype).RawTableName()) + " WHERE " + dialect.QuoteIdent(column) + " = ?"
				if qerr := database.QueryRow(kdb.Rebind(dialect, query), value).Scan(&n); qerr != nil {
					t.Fatalf("count %s: %v", doctype, qerr)
				}
				return n
			}
			outboxRows := func() int {
				t.Helper()
				var n int
				if qerr := database.QueryRow(kdb.Rebind(dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?"), site).Scan(&n); qerr != nil {
					t.Fatalf("count outbox: %v", qerr)
				}
				return n
			}
			itemName := "Sugar 2kg " + suffix
			createArgs := fmt.Sprintf(`{"record_type":"Item","data":{"item_name":%q,"uom":"Each","price":250}}`, itemName)
			confirmArgs := func(token string) string { return fmt.Sprintf(`{"confirm_token":%q}`, token) }

			// Turn 1: the create previews. Nothing is written and no outbox event is kept.
			outboxBefore := outboxRows()
			preview := turn("run-1", alice, "alice@example.test", site, conversation, call("c1", toolCreateRecord, createArgs))
			if !strings.HasPrefix(preview, `{"preview"`) || !strings.Contains(preview, `"confirm_token":"cw_`) {
				t.Fatalf("create preview on %s = %s, want preview with confirm_token", tc.name, preview)
			}
			if n := count("Item", "item_name", itemName); n != 0 {
				t.Fatalf("preview wrote %d Item rows on %s, want 0", n, tc.name)
			}
			if outboxRows() != outboxBefore {
				t.Fatalf("preview changed outbox rows on %s", tc.name)
			}
			var previewed struct {
				ConfirmToken string `json:"confirm_token"`
			}
			if err = json.Unmarshal([]byte(preview), &previewed); err != nil {
				t.Fatal(err)
			}
			token := previewed.ConfirmToken

			// The same turn cannot confirm its own preview.
			if got := turn("run-1", alice, "alice@example.test", site, conversation, call("c2", toolConfirmWrite, confirmArgs(token))); !strings.Contains(got, `"code":"confirm_invalid"`) {
				t.Fatalf("same-turn confirm on %s = %s, want confirm_invalid", tc.name, got)
			}
			// Another person, site or conversation cannot confirm it either.
			if got := turn("run-2", bob, "bob@example.test", site, conversation, call("c3", toolConfirmWrite, confirmArgs(token))); !strings.Contains(got, `"code":"confirm_invalid"`) {
				t.Fatalf("other-person confirm on %s = %s, want confirm_invalid", tc.name, got)
			}
			if got := turn("run-2", alice, "alice@example.test", "other-"+site, conversation, call("c4", toolConfirmWrite, confirmArgs(token))); !strings.Contains(got, `"code":"confirm_invalid"`) {
				t.Fatalf("other-site confirm on %s = %s, want confirm_invalid", tc.name, got)
			}
			if got := turn("run-2", alice, "alice@example.test", site, "other-"+conversation, call("c5", toolConfirmWrite, confirmArgs(token))); !strings.Contains(got, `"code":"confirm_invalid"`) {
				t.Fatalf("other-conversation confirm on %s = %s, want confirm_invalid", tc.name, got)
			}

			// Turn 2: the person said yes. One row is written, with the preview's outcome.
			outcome := turn("run-2", alice, "alice@example.test", site, conversation, call("c6", toolConfirmWrite, confirmArgs(token)))
			if !strings.HasPrefix(outcome, "Created Item") {
				t.Fatalf("confirm on %s = %s, want Created Item", tc.name, outcome)
			}
			if n := count("Item", "item_name", itemName); n != 1 {
				t.Fatalf("confirm wrote %d Item rows on %s, want 1", n, tc.name)
			}
			// A repeated confirm replays the first outcome and writes nothing.
			if replay := turn("run-3", alice, "alice@example.test", site, conversation, call("c7", toolConfirmWrite, confirmArgs(token))); replay != outcome {
				t.Fatalf("replayed confirm on %s = %q, want first outcome %q", tc.name, replay, outcome)
			}
			if n := count("Item", "item_name", itemName); n != 1 {
				t.Fatalf("replayed confirm wrote a second row on %s", tc.name)
			}

			// An expired preview cannot be confirmed.
			expiring := itemName + " expiring"
			expiredPreview := turn("run-4", alice, "alice@example.test", site, conversation, call("c8", toolCreateRecord, fmt.Sprintf(`{"record_type":"Item","data":{"item_name":%q,"uom":"Each","price":100}}`, expiring)))
			expiredToken := tokenOf(t, expiredPreview)
			setPendingCreatedAt(t, database, dialect, expiredToken, time.Now().UTC().Add(-time.Hour))
			if got := turn("run-5", alice, "alice@example.test", site, conversation, call("c9", toolConfirmWrite, confirmArgs(expiredToken))); !strings.Contains(got, `"code":"confirm_expired"`) {
				t.Fatalf("expired confirm on %s = %s, want confirm_expired", tc.name, got)
			}

			// A changed stored argument cannot be confirmed.
			tampered := itemName + " tampered"
			tamperPreview := turn("run-6", alice, "alice@example.test", site, conversation, call("c10", toolCreateRecord, fmt.Sprintf(`{"record_type":"Item","data":{"item_name":%q,"uom":"Each","price":100}}`, tampered)))
			tamperToken := tokenOf(t, tamperPreview)
			setPendingArgs(t, database, dialect, tamperToken, strings.Replace(argsJSONOf(t, database, dialect, tamperToken), `"price":100`, `"price":999`, 1))
			if got := turn("run-7", alice, "alice@example.test", site, conversation, call("c11", toolConfirmWrite, confirmArgs(tamperToken))); !strings.Contains(got, `"code":"confirm_invalid"`) {
				t.Fatalf("tampered confirm on %s = %s, want confirm_invalid", tc.name, got)
			}
			if n := count("Item", "item_name", tampered); n != 0 {
				t.Fatalf("tampered confirm wrote a row on %s", tc.name)
			}

			// A create missing required fields returns one missing_fields error and no token.
			missing := turn("run-8", alice, "alice@example.test", site, conversation, call("c12", toolCreateRecord, `{"record_type":"Item","data":{"uom":"Each"}}`))
			if strings.Count(missing, `"code":"missing_fields"`) != 1 || strings.Contains(missing, "confirm_token") {
				t.Fatalf("missing-field create on %s = %s, want one missing_fields error and no token", tc.name, missing)
			}
		})
	}
}

// isolatedAIDatabase creates a uniquely named database for one run, so the shared
// dialect DSN database does not hold tables from other tests, and drops it at cleanup.
func isolatedAIDatabase(t *testing.T, driver, dsn string) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("kora_ai_pw_%x", time.Now().UnixNano())
	admin, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open %s admin connection: %v", driver, err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if driver == "mysql" {
		cfg, perr := mysqlDriverConfig(dsn)
		if perr != nil {
			t.Fatal(perr)
		}
		cfg.DBName = name
		if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			t.Fatal("create isolated MySQL database:", err)
		}
		t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`") })
		database, oerr := sql.Open(driver, cfg.FormatDSN())
		if oerr != nil {
			t.Fatal("open isolated MySQL database:", oerr)
		}
		t.Cleanup(func() { _ = database.Close() })
		return database
	}
	if _, cerr := admin.Exec(`CREATE DATABASE "` + name + `"`); cerr != nil {
		t.Fatal("create isolated Postgres database:", cerr)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`) })
	database, oerr := sql.Open(driver, postgresDSNWithDatabase(t, dsn, name))
	if oerr != nil {
		t.Fatal("open isolated Postgres database:", oerr)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// tokenOf returns the confirm_token of a preview result.
func tokenOf(t *testing.T, preview string) string {
	t.Helper()
	var out struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := json.Unmarshal([]byte(preview), &out); err != nil || out.ConfirmToken == "" {
		t.Fatalf("preview %s has no confirm_token: %v", preview, err)
	}
	return out.ConfirmToken
}

// setPendingCreatedAt moves a stored preview's creation time, to test expiry against the database clock.
func setPendingCreatedAt(t *testing.T, database *sql.DB, dialect kdb.Dialect, token string, at time.Time) {
	t.Helper()
	if _, err := database.Exec(kdb.Rebind(dialect, "UPDATE _kora_ai_pending_write SET created_at = ? WHERE id = ?"), at, pendingIDFor(token)); err != nil {
		t.Fatal("age pending write:", err)
	}
}

func setPendingArgs(t *testing.T, database *sql.DB, dialect kdb.Dialect, token, args string) {
	t.Helper()
	if _, err := database.Exec(kdb.Rebind(dialect, "UPDATE _kora_ai_pending_write SET args_json = ? WHERE id = ?"), args, pendingIDFor(token)); err != nil {
		t.Fatal("change pending write arguments:", err)
	}
}

func argsJSONOf(t *testing.T, database *sql.DB, dialect kdb.Dialect, token string) string {
	t.Helper()
	var args string
	if err := database.QueryRow(kdb.Rebind(dialect, "SELECT args_json FROM _kora_ai_pending_write WHERE id = ?"), pendingIDFor(token)).Scan(&args); err != nil {
		t.Fatal("read pending write arguments:", err)
	}
	return args
}

func mysqlDriverConfig(dsn string) (*mysql.Config, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse MySQL DSN: %w", err)
	}
	return cfg, nil
}

// postgresDSNWithDatabase returns the URL form of dsn with its database path replaced.
func postgresDSNWithDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse Postgres DSN:", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}
