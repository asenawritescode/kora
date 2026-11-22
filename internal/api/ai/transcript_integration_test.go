//go:build integration

package ai

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// TestTranscriptCarriesRunIDsAcrossDialects checks that the transcript and its answer attachments restore with the run
// that wrote each entry, on MySQL and Postgres. The DSNs name disposable databases; each case uses its own site.
func TestTranscriptCarriesRunIDsAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
	}{
		{name: "mysql", dsn: "KORA_AI_TRANSCRIPT_MYSQL_DSN"},
		{name: "postgres", dsn: "KORA_AI_TRANSCRIPT_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.dsn)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.dsn, tc.name)
			}
			if tc.name == "mysql" {
				dsn = isolatedMySQLDSN(t, dsn)
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
			ctx := WithDialect(t.Context(), dialect)
			if err = EnsureAIRunTables(ctx, database, dialect); err != nil {
				t.Fatalf("bootstrap %s AI tables: %v", tc.name, err)
			}
			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			site, conversationID := "transcript-"+tc.name+"-"+suffix, "conversation-"+suffix
			t.Cleanup(func() {
				_, _ = database.Exec(kdb.Rebind(dialect, "DELETE FROM _kora_ai_message WHERE site = ?"), site)
			})

			appends := []MessageRecord{
				{Site: site, ConversationID: conversationID, RunID: "run-1", Role: "user", Content: "sales last week?", Kind: "message"},
				{Site: site, ConversationID: conversationID, RunID: "run-1", Role: "assistant", Content: "Here are sales.", Kind: "summary"},
				{Site: site, ConversationID: conversationID, RunID: "run-1", Role: "assistant", Content: `{"cards":[]}`, Kind: "card"},
				{Site: site, ConversationID: conversationID, RunID: "run-2", Role: "user", Content: "and the week before?", Kind: "message"},
			}
			for _, msg := range appends {
				if err = AppendConversationMessage(ctx, database, msg); err != nil {
					t.Fatalf("append %s message: %v", tc.name, err)
				}
			}

			messages, err := ListConversationMessages(ctx, database, site, conversationID)
			if err != nil {
				t.Fatalf("list %s transcript: %v", tc.name, err)
			}
			wantRuns := []string{"run-1", "run-1", "run-2"}
			if len(messages) != len(wantRuns) {
				t.Fatalf("%s transcript has %d messages, want %d: %#v", tc.name, len(messages), len(wantRuns), messages)
			}
			for i, m := range messages {
				if m.RunID != wantRuns[i] {
					t.Fatalf("%s transcript message %d run_id = %q, want %q", tc.name, i, m.RunID, wantRuns[i])
				}
			}

			attachments, err := ListAnswerAttachments(ctx, database, site, conversationID)
			if err != nil {
				t.Fatalf("list %s attachments: %v", tc.name, err)
			}
			if len(attachments) != 1 || attachments[0].RunID != "run-1" || attachments[0].Kind != answerKindCard {
				t.Fatalf("%s attachments = %#v, want one card of run-1", tc.name, attachments)
			}
		})
	}
}

// isolatedMySQLDSN creates a uniquely named database on the root DSN and drops it in Cleanup. CI's disposable DSN
// names no database, so the test must make its own, as the projection tests do.
func isolatedMySQLDSN(t *testing.T, root string) string {
	t.Helper()
	config, err := mysqldriver.ParseDSN(root)
	if err != nil {
		t.Fatalf("parse mysql DSN: %v", err)
	}
	name := fmt.Sprintf("kora_ai_transcript_%x", time.Now().UnixNano())
	admin, err := sql.Open("mysql", root)
	if err != nil {
		t.Fatalf("open mysql admin connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create isolated mysql database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`") })
	config.DBName = name
	return config.FormatDSN()
}
