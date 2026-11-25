//go:build integration

package ai

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
)

// TestCleanupExpiredRemovesAuditRowsAcrossDialects checks that retention removes the audit rows of the runs and
// conversations it deletes, and keeps the audit rows of live ones, on MySQL and Postgres.
func TestCleanupExpiredRemovesAuditRowsAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
	}{
		{name: "mysql", dsn: "KORA_AI_RETENTION_MYSQL_DSN"},
		{name: "postgres", dsn: "KORA_AI_RETENTION_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.dsn)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.dsn, tc.name)
			}
			database := disposabledb.Open(t, tc.name, dsn).DB
			dialect := kdb.Resolve(tc.name)
			ctx := WithDialect(t.Context(), dialect)
			if err := EnsureAIRunTables(ctx, database, dialect); err != nil {
				t.Fatalf("bootstrap %s AI tables: %v", tc.name, err)
			}

			now := time.Now().UTC().Truncate(time.Second)
			past, future := now.Add(-time.Hour), now.Add(24*time.Hour)
			site := fmt.Sprintf("retention-%s-%x", tc.name, now.UnixNano())
			liveConv := ConversationRecord{ID: "conv-live-" + site, Site: site, Channel: "chat", SubjectKey: "live-" + site, Status: "active", RetentionExpiresAt: future}
			expiredConv := ConversationRecord{ID: "conv-expired-" + site, Site: site, Channel: "chat", SubjectKey: "expired-" + site, Status: "active", RetentionExpiresAt: past}
			for _, conv := range []ConversationRecord{liveConv, expiredConv} {
				if err := UpsertConversation(ctx, database, conv); err != nil {
					t.Fatalf("insert %s conversation: %v", tc.name, err)
				}
			}
			runs := []RunRecord{
				{ID: "run-live-" + site, Site: site, ConversationID: liveConv.ID, Status: "completed", RetentionExpiresAt: future},
				{ID: "run-expired-" + site, Site: site, ConversationID: liveConv.ID, Status: "completed", RetentionExpiresAt: past},
				{ID: "run-in-expired-conv-" + site, Site: site, ConversationID: expiredConv.ID, Status: "completed", RetentionExpiresAt: future},
			}
			for _, run := range runs {
				if err := UpsertRun(ctx, database, run); err != nil {
					t.Fatalf("insert %s run %s: %v", tc.name, run.ID, err)
				}
			}

			audits := []AuditEvent{
				{ID: "audit-live-run-" + site, Site: site, RunID: runs[0].ID, ConversationID: liveConv.ID, Kind: "tool_call", Status: "completed"},
				{ID: "audit-live-conv-" + site, Site: site, ConversationID: liveConv.ID, Kind: "tool_call", Status: "completed"},
				{ID: "audit-expired-run-" + site, Site: site, RunID: runs[1].ID, ConversationID: liveConv.ID, Kind: "tool_call", Status: "completed"},
				{ID: "audit-in-expired-conv-" + site, Site: site, RunID: runs[2].ID, ConversationID: expiredConv.ID, Kind: "tool_call", Status: "completed"},
				{ID: "audit-expired-conv-" + site, Site: site, ConversationID: expiredConv.ID, Kind: "tool_call", Status: "completed"},
			}
			for _, ev := range audits {
				if err := RecordAudit(ctx, database, ev); err != nil {
					t.Fatalf("record %s audit %s: %v", tc.name, ev.ID, err)
				}
			}

			if _, err := CleanupExpired(ctx, database, now); err != nil {
				t.Fatalf("cleanup %s expired AI rows: %v", tc.name, err)
			}

			rows, err := database.QueryContext(ctx, kdb.Rebind(dialect, `SELECT id FROM _kora_ai_audit WHERE site = ? ORDER BY id`), site)
			if err != nil {
				t.Fatalf("list %s audit rows: %v", tc.name, err)
			}
			defer rows.Close()
			var remaining []string
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					t.Fatalf("scan %s audit row: %v", tc.name, err)
				}
				remaining = append(remaining, id)
			}
			if err = rows.Err(); err != nil {
				t.Fatalf("read %s audit rows: %v", tc.name, err)
			}
			want := []string{"audit-live-conv-" + site, "audit-live-run-" + site}
			slices.Sort(want)
			if !slices.Equal(remaining, want) {
				t.Fatalf("%s audit rows after cleanup = %v, want %v", tc.name, remaining, want)
			}
		})
	}
}
