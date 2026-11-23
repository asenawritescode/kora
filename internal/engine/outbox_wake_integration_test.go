//go:build integration

package engine

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/sweep"
)

// recordingPublisher keeps every event the outbox publisher delivers.
type recordingPublisher struct {
	mu        sync.Mutex
	published []contract.EventEnvelope
}

func (p *recordingPublisher) Publish(_ context.Context, event contract.EventEnvelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, event)
	return nil
}

func (p *recordingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}

// TestSavedRecordOutboxRowPublishedPromptly saves a record through the ORM and
// runs the commit's post-commit hooks, as the kernel does. The publisher's
// sweep is the production 5 s, so only the wake can publish the row within
// the one-second bound.
func TestSavedRecordOutboxRowPublishedPromptly(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s service", tc.env, tc.dialect)
			}
			database := disposabledb.Open(t, tc.dialect, dsn)
			conn := database.DB
			d := kdb.Resolve(tc.dialect)
			ddl := d.SystemTableSQL()
			if tc.dialect == "mysql" {
				ddl = append(ddl, kdb.OutboxTablesMySQL()...)
			} else {
				ddl = append(ddl, kdb.OutboxTablesPostgres()...)
			}
			for _, stmt := range ddl {
				if !strings.HasPrefix(strings.TrimSpace(stmt), "CREATE TABLE IF NOT EXISTS") {
					continue
				}
				if _, err := conn.Exec(stmt); err != nil {
					t.Fatalf("system tables: %v", err)
				}
			}
			dt := &doctype.DocType{Name: "Visit", Module: "T", Fields: []doctype.Field{
				{Fieldname: "code", Fieldtype: "Data", Label: "Code"},
			}}
			reg := doctype.NewRegistry()
			reg.LoadFull([]*doctype.DocType{dt}, nil, nil)
			if err := schema.MigrateSiteFromRegistry(conn, "", reg, d); err != nil {
				t.Fatalf("create visit table: %v", err)
			}

			wake := sweep.NewWake()
			dest := &recordingPublisher{}
			publisher := outbox.NewPublisher(conn, dest, d)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				RunOutboxPublisher(ctx, publisher, nil, wake.Subscribe())
			}()

			tx := &orm.TxManager{DB: conn, Registry: reg, Dialect: d, SiteName: "wake-it",
				Outbox: outbox.NewSQLWriter(d), OutboxWake: wake}
			doc := doctype.NewDocument("Visit")
			doc.Set("code", "V-1")
			if err := tx.PrepareInsert(dt, doc); err != nil {
				t.Fatal(err)
			}
			dbTx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.InsertInTx(dbTx, dt, doc, "u1", "u1"); err != nil {
				_ = dbTx.Rollback()
				t.Fatal(err)
			}
			if err := dbTx.Commit(); err != nil {
				t.Fatal(err)
			}
			committed := time.Now()
			for _, hook := range tx.PostCommitHooks {
				hook()
			}

			for dest.count() == 0 {
				if time.Since(committed) > time.Second {
					t.Fatalf("outbox row not published within 1s of commit (sweep is 5s)")
				}
				time.Sleep(5 * time.Millisecond)
			}
			t.Logf("outbox row published %v after commit (sweep 5s)", time.Since(committed))
			cancel()
			<-done
		})
	}
}
