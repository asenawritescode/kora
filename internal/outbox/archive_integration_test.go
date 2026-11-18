//go:build integration

package outbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/storage"
)

// memArchive keeps archive objects in memory. err, when set, fails every Put.
type memArchive struct {
	err     error
	objects map[string][]byte
}

func newMemArchive() *memArchive { return &memArchive{objects: map[string][]byte{}} }

func (m *memArchive) Put(_ context.Context, key string, r io.Reader, _ int64, _ storage.FileMeta) error {
	if m.err != nil {
		return m.err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.objects[key] = body
	return nil
}

func TestMySQLOutboxArchive(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_OUTBOX_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_OUTBOX_DISPOSABLE_MYSQL_DSN to a disposable MySQL service")
	}
	runArchiveSuite(t, "mysql", dsn, db.Resolve("mysql"), db.OutboxTablesMySQL())
}

func TestPostgresOutboxArchive(t *testing.T) {
	dsn := os.Getenv("KORA_OUTBOX_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_OUTBOX_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL service")
	}
	runArchiveSuite(t, "postgres", dsn, db.Resolve("postgres"), db.OutboxTablesPostgres())
}

// runArchiveSuite gives every scenario its own database, so no scenario sees the
// rows another one left behind.
func runArchiveSuite(t *testing.T, driver, dsn string, dialect db.Dialect, ddl []string) {
	scenarios := map[string]func(t *testing.T, s archiveFixture){
		"archives old published rows with their receipts": scenarioArchivesOldRows,
		"leaves every other state untouched":              scenarioLeavesOtherRows,
		"keeps rows when the archive write fails":         scenarioKeepsRowsOnPutFailure,
		"rerun after a crash writes the same key":         scenarioRerunAfterCrash,
		"cutoff holds in a UTC+14 session zone":           sessionZoneScenario("Pacific/Kiritimati", "+14:00"),
		"cutoff holds in a UTC-12 session zone":           sessionZoneScenario("Etc/GMT+12", "-12:00"),
		"probe finds only archivable rows":                scenarioProbeFindsDueRows,
	}
	for name, run := range scenarios {
		t.Run(name, func(t *testing.T) {
			database := disposabledb.Open(t, driver, dsn)
			for _, statement := range ddl {
				if _, err := database.DB.Exec(statement); err != nil {
					t.Fatal("create outbox tables:", err)
				}
			}
			run(t, archiveFixture{db: database.DB, dialect: dialect})
		})
	}
}

type archiveFixture struct {
	db      *sql.DB
	dialect db.Dialect
}

func (f archiveFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(db.Rebind(f.dialect, query), args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func (f archiveFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(db.Rebind(f.dialect, query), args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// insertRow writes one outbox row in the given status, created at created.
func (f archiveFixture) insertRow(t *testing.T, id, status string, created time.Time) {
	t.Helper()
	var published any
	if status == string(StatusPublished) {
		published = created.Add(time.Hour)
	}
	f.exec(t, `INSERT INTO _kora_outbox (id, site, event_type, status, created_at, published_at, payload) VALUES (?, 'acme', 'kora.test.created', ?, ?, ?, ?)`,
		id, status, created, published, `{"id":"`+id+`"}`)
}

func (f archiveFixture) insertReceipt(t *testing.T, consumer, id string) {
	t.Helper()
	f.exec(t, `INSERT INTO _kora_outbox_receipt (consumer_name, event_id, site, received_at) VALUES (?, ?, 'acme', ?)`, consumer, id, time.Now().UTC())
}

func (f archiveFixture) archiver(t *testing.T, store ArchiveStore, metrics *BacklogMetrics, chunk int) *Archiver {
	t.Helper()
	a, err := NewArchiver(Archiver{DB: f.db, Dialect: f.dialect, Store: store, Horizon: MinArchiveHorizon, ChunkSize: chunk, Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func oldTime() time.Time { return time.Now().UTC().Add(-200 * 24 * time.Hour) }

func scenarioArchivesOldRows(t *testing.T, f archiveFixture) {
	f.insertRow(t, "old-1", "published", oldTime())
	f.insertRow(t, "old-2", "published", oldTime())
	f.insertRow(t, "old-3", "published", oldTime())
	f.insertRow(t, "recent", "published", time.Now().UTC().Add(-24*time.Hour))
	for _, id := range []string{"old-1", "old-2", "old-3", "recent"} {
		f.insertReceipt(t, "webhooks", id)
	}
	store, metrics := newMemArchive(), &BacklogMetrics{}
	n, err := f.archiver(t, store, metrics, 2).ArchiveDue(context.Background(), "acme")
	if err != nil || n != 3 {
		t.Fatalf("ArchiveDue = %d, %v; want 3, nil", n, err)
	}
	if len(store.objects) != 2 {
		t.Fatalf("archive objects = %d, want 2 (chunk size 2 over 3 rows)", len(store.objects))
	}
	for key := range store.objects {
		if !strings.HasPrefix(key, "outbox-archive/acme/") || !strings.HasSuffix(key, ".ndjson.gz") {
			t.Fatalf("unexpected key %q", key)
		}
	}
	if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox`); got != 1 {
		t.Fatalf("outbox rows left = %d, want only the recent row", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox_receipt WHERE event_id LIKE 'old-%'`); got != 0 {
		t.Fatalf("archived receipts left = %d, want 0", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox_receipt WHERE event_id = 'recent'`); got != 1 {
		t.Fatalf("recent receipt left = %d, want 1", got)
	}
	snapshot := metrics.Snapshot(time.Now())
	if snapshot.ArchivedRows != 3 || snapshot.ArchiveFailures != 0 {
		t.Fatalf("metrics archived=%d failures=%d, want 3 and 0", snapshot.ArchivedRows, snapshot.ArchiveFailures)
	}
}

func scenarioLeavesOtherRows(t *testing.T, f archiveFixture) {
	old := oldTime()
	f.insertRow(t, "pending", "pending", old)
	f.insertRow(t, "blocked", "blocked", old)
	f.insertRow(t, "failed", "failed", old)
	f.insertRow(t, "recent", "published", time.Now().UTC().Add(-24*time.Hour))
	f.insertRow(t, "quarantined", "published", old)
	f.exec(t, `INSERT INTO _kora_outbox_delivery_failure (consumer_name, event_id, site, attempts, quarantined, last_error, updated_at) VALUES ('webhooks', 'quarantined', 'acme', 5, ?, 'boom', ?)`, true, time.Now().UTC())
	store := newMemArchive()
	n, err := f.archiver(t, store, nil, 10).ArchiveDue(context.Background(), "acme")
	if err != nil || n != 0 || len(store.objects) != 0 {
		t.Fatalf("ArchiveDue = %d, %v with %d objects; want nothing archived", n, err, len(store.objects))
	}
	if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox`); got != 5 {
		t.Fatalf("outbox rows = %d, want all 5 kept", got)
	}
}

func scenarioKeepsRowsOnPutFailure(t *testing.T, f archiveFixture) {
	f.insertRow(t, "old-1", "published", oldTime())
	f.insertReceipt(t, "webhooks", "old-1")
	store := newMemArchive()
	store.err = errors.New("bucket unavailable")
	metrics := &BacklogMetrics{}
	if _, err := f.archiver(t, store, metrics, 10).ArchiveDue(context.Background(), "acme"); err == nil {
		t.Fatal("ArchiveDue succeeded although the archive write failed")
	}
	if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox`); got != 1 {
		t.Fatalf("outbox rows = %d after failed Put, want 1", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox_receipt`); got != 1 {
		t.Fatalf("receipts = %d after failed Put, want 1", got)
	}
	if snapshot := metrics.Snapshot(time.Now()); snapshot.ArchiveFailures != 1 || snapshot.ArchivedRows != 0 {
		t.Fatalf("metrics failures=%d archived=%d, want 1 and 0", snapshot.ArchiveFailures, snapshot.ArchivedRows)
	}
}

func scenarioRerunAfterCrash(t *testing.T, f archiveFixture) {
	f.insertRow(t, "old-1", "published", oldTime())
	f.insertRow(t, "old-2", "published", oldTime())
	f.insertReceipt(t, "webhooks", "old-1")
	store := newMemArchive()
	a := f.archiver(t, store, nil, 10)
	// Simulate a crash after Put and before the delete transaction: the object is
	// written, the rows stay.
	rows, created, err := a.selectDue(context.Background(), "acme")
	if err != nil || len(rows) != 2 {
		t.Fatalf("selectDue = %d rows, %v; want 2", len(rows), err)
	}
	body, err := encodeArchive(rows)
	if err != nil {
		t.Fatal(err)
	}
	key := archiveKey("acme", created, rows)
	if err := store.Put(context.Background(), key, bytes.NewReader(body.Bytes()), int64(body.Len()), storage.FileMeta{}); err != nil {
		t.Fatal(err)
	}
	if n, err := a.ArchiveDue(context.Background(), "acme"); err != nil || n != 2 {
		t.Fatalf("rerun ArchiveDue = %d, %v; want 2, nil", n, err)
	}
	if _, ok := store.objects[key]; !ok || len(store.objects) != 1 {
		t.Fatalf("rerun left keys %v, want exactly %q", keysOf(store.objects), key)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox`); got != 0 {
		t.Fatalf("outbox rows = %d after rerun, want 0", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox_receipt`); got != 0 {
		t.Fatalf("receipts = %d after rerun, want 0", got)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sessionZoneScenario sets the session time zone (PostgreSQL name, MySQL offset)
// and checks the horizon boundary: a row one hour past the horizon is archived,
// one hour inside it is kept. created_at is stamped by the database in that
// session zone, so a cutoff bound from UTC would move the boundary by the zone
// offset and fail this check.
func sessionZoneScenario(pgZone, mysqlZone string) func(t *testing.T, f archiveFixture) {
	return func(t *testing.T, f archiveFixture) {
		f.db.SetMaxOpenConns(1)
		f.db.SetMaxIdleConns(1)
		if f.dialect.DriverName() == "postgres" {
			f.exec(t, "SET TIME ZONE '"+pgZone+"'")
		} else {
			f.exec(t, "SET time_zone = '"+mysqlZone+"'")
		}
		insertAged := func(id string, extra time.Duration) {
			f.exec(t, `INSERT INTO _kora_outbox (id, site, event_type, status, created_at, published_at, payload) VALUES (?, 'acme', 'kora.test.created', 'published', `+f.dialect.NowMinusSeconds()+`, NULL, ?)`,
				id, archiveHorizonSeconds(MinArchiveHorizon+extra), `{"id":"`+id+`"}`)
		}
		insertAged("past-horizon", time.Hour)
		insertAged("inside-horizon", -time.Hour)
		store := newMemArchive()
		if n, err := f.archiver(t, store, nil, 10).ArchiveDue(context.Background(), "acme"); err != nil || n != 1 {
			t.Fatalf("ArchiveDue = %d, %v; want 1, nil", n, err)
		}
		if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE id = ?`, "inside-horizon"); got != 1 {
			t.Fatalf("inside-horizon row kept = %d, want 1", got)
		}
		if got := f.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE id = ?`, "past-horizon"); got != 0 {
			t.Fatalf("past-horizon row archived = %d rows left, want 0", got)
		}
	}
}

// scenarioProbeFindsDueRows checks that the scanner probe answers the same
// question as the archiver: old published rows yes, recent or failed rows no.
func scenarioProbeFindsDueRows(t *testing.T, f archiveFixture) {
	probe := func() bool {
		query, args := ArchiveDueProbe(f.dialect, MinArchiveHorizon)
		var one int
		err := f.db.QueryRow(query, args...).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("probe:", err)
		}
		return err == nil
	}
	f.insertRow(t, "recent", "published", time.Now().UTC().Add(-24*time.Hour))
	if probe() {
		t.Fatal("probe reported due work for a recent published row")
	}
	f.insertRow(t, "old", "published", oldTime())
	f.insertRow(t, "old-failed", "published", oldTime())
	f.exec(t, `INSERT INTO _kora_outbox_delivery_failure (consumer_name, event_id, site, attempts, quarantined, last_error, updated_at) VALUES ('webhooks', 'old-failed', 'acme', 5, ?, 'boom', ?)`, true, time.Now().UTC())
	if !probe() {
		t.Fatal("probe missed an old published row")
	}
	f.exec(t, `DELETE FROM _kora_outbox WHERE id = ?`, "old")
	if probe() {
		t.Fatal("probe reported a row kept by a delivery failure")
	}
}
