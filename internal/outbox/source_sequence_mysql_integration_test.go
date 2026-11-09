//go:build integration

package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLSourceSequenceCommitsWithOutboxTransaction(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_OUTBOX_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_OUTBOX_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	if rootConfig.Net != "tcp" {
		t.Fatal("disposable outbox test requires loopback TCP MySQL")
	}
	host, _, err := net.SplitHostPort(rootConfig.Addr)
	if err != nil {
		t.Fatal("parse MySQL address:", err)
	}
	if !strings.EqualFold(host, "localhost") {
		address, parseErr := netip.ParseAddr(strings.Trim(host, "[]"))
		if parseErr != nil || !address.IsLoopback() {
			t.Fatal("refusing outbox integration test against non-loopback MySQL")
		}
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("disposable MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_outbox_sequence_%d", time.Now().UnixNano())
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = root.Close()
	})
	databaseConfig := *rootConfig
	databaseConfig.DBName = name
	database, err := sql.Open("mysql", databaseConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	dialect := db.Resolve("mysql")
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create outbox schema: %v\n%s", err, statement)
		}
	}
	writer := NewSQLWriter(dialect)
	appendEvent := func(site string) contract.EventEnvelope {
		t.Helper()
		tx, err := database.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal("begin event transaction:", err)
		}
		event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "record.updated", Source: "test", Site: site, AggregateType: "Record", AggregateID: contract.NewID(), Data: json.RawMessage(`{}`)}
		if err := writer.Append(context.Background(), tx, event); err != nil {
			_ = tx.Rollback()
			t.Fatal("append event:", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal("commit event:", err)
		}
		return event
	}
	first := appendEvent("tenant-a")
	var firstPayload string
	if err := database.QueryRow(`SELECT payload FROM _kora_outbox WHERE id = ?`, first.ID).Scan(&firstPayload); err != nil {
		t.Fatal("read first outbox payload:", err)
	}
	var firstStored contract.EventEnvelope
	if err := json.Unmarshal([]byte(firstPayload), &firstStored); err != nil || firstStored.Sequence != "1" {
		t.Fatalf("first committed envelope sequence = %q (unmarshal error %v), want 1", firstStored.Sequence, err)
	}
	rolledBack, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin rollback transaction:", err)
	}
	if err := writer.Append(context.Background(), rolledBack, contract.EventEnvelope{Type: "record.updated", Source: "test", Site: "tenant-a", AggregateType: "Record", AggregateID: contract.NewID(), Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal("append rolled-back event:", err)
	}
	if err := rolledBack.Rollback(); err != nil {
		t.Fatal("rollback event:", err)
	}
	third := appendEvent("tenant-a")
	var thirdPayload string
	if err := database.QueryRow(`SELECT payload FROM _kora_outbox WHERE id = ?`, third.ID).Scan(&thirdPayload); err != nil {
		t.Fatal("read next outbox payload:", err)
	}
	var thirdStored contract.EventEnvelope
	if err := json.Unmarshal([]byte(thirdPayload), &thirdStored); err != nil || thirdStored.Sequence != "2" {
		t.Fatalf("post-rollback envelope sequence = %q (unmarshal error %v), want 2", thirdStored.Sequence, err)
	}
	var outboxRows, lastSequence int
	if err := database.QueryRow(`SELECT COUNT(*), MAX(source_sequence) FROM _kora_outbox WHERE site = ?`, "tenant-a").Scan(&outboxRows, &lastSequence); err != nil {
		t.Fatal("read committed outbox sequences:", err)
	}
	if outboxRows != 2 || lastSequence != 2 {
		t.Fatalf("committed outbox rows/max sequence = %d/%d, want 2/2", outboxRows, lastSequence)
	}
	// A route rename must not create a second sequence domain. This row models
	// a legacy route-keyed sequence left by the prior implementation; the
	// singleton seeds from its maximum before assigning the new-route event.
	if _, err := database.Exec(`INSERT INTO _kora_event_sequence (site, last_sequence) VALUES (?, ?)`, "old-route", 8); err != nil {
		t.Fatal("insert legacy route sequence:", err)
	}
	renamed := appendEvent("new-route")
	var renamedPayload string
	if err := database.QueryRow(`SELECT payload FROM _kora_outbox WHERE id = ?`, renamed.ID).Scan(&renamedPayload); err != nil {
		t.Fatal("read renamed-route outbox payload:", err)
	}
	var renamedStored contract.EventEnvelope
	if err := json.Unmarshal([]byte(renamedPayload), &renamedStored); err != nil || renamedStored.Sequence != "9" || renamedStored.Site != "new-route" {
		t.Fatalf("renamed-route envelope = sequence %q/site %q (unmarshal error %v), want 9/new-route", renamedStored.Sequence, renamedStored.Site, err)
	}
}
