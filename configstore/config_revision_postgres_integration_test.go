//go:build integration

package configstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	_ "github.com/lib/pq"
)

// TestPostgresConfigRevisionFenceSerializesConcurrentWrite exercises the
// revision row lock against a disposable PostgreSQL database.
func TestPostgresConfigRevisionFenceSerializesConcurrentWrite(t *testing.T) {
	dsn := os.Getenv("KORA_CONFIG_REVISION_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_CONFIG_REVISION_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var probe int
	var probeErr error
	for attempt := 0; attempt < 100; attempt++ {
		probeErr = database.QueryRowContext(ctx, `SELECT 1`).Scan(&probe)
		if probeErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("probe disposable PostgreSQL database: %v (deadline: %v)", probeErr, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if probeErr != nil || probe != 1 {
		t.Fatalf("probe disposable PostgreSQL database returned %d, err=%v", probe, probeErr)
	}
	dialect := db.Resolve("postgres")
	for _, statement := range ConfigRevisionSchema(dialect) {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal("create config revision table:", err)
		}
	}
	if err := SeedConfigRevision(ctx, database, dialect); err != nil {
		t.Fatal("seed config revision:", err)
	}
	revision, err := CurrentConfigRevision(ctx, database, dialect)
	if err != nil {
		t.Fatal("read selected config revision:", err)
	}

	fenceTx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin transition transaction:", err)
	}
	defer fenceTx.Rollback()
	if err := FenceConfigRevisionTx(ctx, fenceTx, dialect, revision); err != nil {
		t.Fatal("hold selected config revision:", err)
	}

	started := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeTx, err := database.BeginTx(ctx, nil)
		if err != nil {
			writeDone <- err
			return
		}
		defer writeTx.Rollback()
		close(started)
		if err := BumpConfigRevisionTx(writeTx, dialect); err != nil {
			writeDone <- err
			return
		}
		writeDone <- writeTx.Commit()
	}()
	<-started
	select {
	case err := <-writeDone:
		t.Fatalf("PostgreSQL config write completed while fence was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := fenceTx.Commit(); err != nil {
		t.Fatal("commit fenced transition:", err)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal("config write after fence release:", err)
		}
	case <-ctx.Done():
		t.Fatal("config write remained blocked after fence release:", ctx.Err())
	}
	finalRevision, err := CurrentConfigRevision(ctx, database, dialect)
	if err != nil || finalRevision != revision+1 {
		t.Fatalf("final PostgreSQL config revision = %d, err=%v; want %d", finalRevision, err, revision+1)
	}
}
