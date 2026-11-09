//go:build integration

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/site"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// These acceptance tests use the same command entry point as the MySQL test,
// but against disposable PostgreSQL and actual remote LibSQL databases. The
// configured database must be empty because the summary intentionally scans
// the complete authoritative directory.
func TestLivePostgresSiteDirectoryAssignmentSummary(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_SITE_DIRECTORY_SUMMARY_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_SUMMARY_POSTGRES_DSN to a fresh disposable PostgreSQL database")
	}
	assertLiveDirectoryAssignmentSummary(t, "postgres", dsn, 502)
}

func TestLiveLibSQLSiteDirectoryAssignmentSummary(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_SITE_DIRECTORY_SUMMARY_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_SUMMARY_LIBSQL_DSN to a fresh disposable remote LibSQL database")
	}
	assertLiveDirectoryAssignmentSummary(t, "libsql", dsn, 24)
}

func assertLiveDirectoryAssignmentSummary(t *testing.T, driver, dsn string, entries int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open disposable %s platform registry: %v", driver, err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping disposable %s platform registry: %v", driver, err)
	}
	dialect := kdb.Resolve(driver)
	if err := site.BootstrapPlatformRegistry(ctx, database, dialect); err != nil {
		t.Fatalf("bootstrap disposable %s platform registry: %v", driver, err)
	}
	registry := site.NewSQLSiteRegistry(database, driver)
	want := make(map[directoryAssignmentKey]uint64)
	privateIDs := make([]string, 0, entries)
	for i := 0; i < entries; i++ {
		hostname := fmt.Sprintf("directory-summary-%s-%03d.example.invalid", driver, i)
		if err := registry.Upsert(&site.Config{
			Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql",
			DBHost: "127.0.0.1", DBPort: 3306, DBName: "never-opened-tenant-db",
		}); err != nil {
			t.Fatalf("insert %s directory row %d: %v", driver, i, err)
		}
		info, err := registry.ResolveAlias(hostname)
		if err != nil {
			t.Fatalf("resolve %s directory row %d: %v", driver, i, err)
		}
		privateIDs = append(privateIDs, info.SiteID)
		placement := ""
		switch i % 3 {
		case 0:
			placement = "deployment-east"
		case 1:
			placement = "deployment-west"
		case 2:
			if i%6 == 5 {
				placement = "default"
			}
		}
		if placement != "" {
			if _, err := registry.SetRuntimeCellID(info.SiteID, placement); err != nil {
				t.Fatalf("assign %s directory row %d: %v", driver, i, err)
			}
		}
		status := "active"
		if i%2 == 1 {
			status = "provisioning"
			if _, err := registry.SetStatusContext(ctx, info.SiteID, status); err != nil {
				t.Fatalf("set %s directory status %d: %v", driver, i, err)
			}
		}
		placement, err = site.NormalizeRuntimeCellID(placement)
		if err != nil {
			t.Fatalf("normalize expected %s placement: %v", driver, err)
		}
		if placement == "" {
			placement = "default"
		}
		want[directoryAssignmentKey{deploymentID: placement, status: status}]++
	}

	t.Setenv("KORA_DB_TYPE", driver)
	t.Setenv("DB_DSN", dsn)
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetContext(ctx)
	if err := runSiteDirectoryAssignmentSummary(command, nil); err != nil {
		t.Fatalf("run %s SQL-backed assignment summary: %v", driver, err)
	}
	var got directoryAssignmentSummary
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode %s aggregate summary: %v", driver, err)
	}
	if got.Version != 1 || got.PlacementSource != "runtime_cell_id" || got.DirectoryRevision == 0 || got.TotalEntries != uint64(entries) {
		t.Fatalf("%s summary metadata = %+v, want version 1, v1 source, revision, and %d rows", driver, got, entries)
	}
	if len(got.Assignments) != len(want) {
		t.Fatalf("%s summary buckets = %+v, want %d buckets", driver, got.Assignments, len(want))
	}
	for _, bucket := range got.Assignments {
		key := directoryAssignmentKey{deploymentID: bucket.DeploymentID, status: bucket.Status}
		if bucket.Count != want[key] {
			t.Fatalf("%s summary bucket %+v count=%d, want %d", driver, key, bucket.Count, want[key])
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("%s summary omitted expected buckets: %+v", driver, want)
	}
	for _, private := range privateIDs {
		if strings.Contains(output.String(), private) {
			t.Fatalf("%s aggregate output leaked tenant identity %q", driver, private)
		}
	}
	if strings.Contains(output.String(), "directory-summary-") || strings.Contains(output.String(), "never-opened-tenant-db") {
		t.Fatalf("%s aggregate output leaked aliases or tenant connection metadata: %s", driver, output.String())
	}
	assertStaleDirectoryAssignmentRejected(t, ctx, registry, privateIDs[0])
	assertConcurrentDirectoryAssignmentConflict(t, ctx, registry, privateIDs[1])
}

type revisionFencedDirectoryAssignment interface {
	GetDescriptorByIDContext(context.Context, string) (site.Descriptor, error)
	SetRuntimeCellIDAtRevisionContext(context.Context, string, string, uint64) (site.Descriptor, error)
}

func assertStaleDirectoryAssignmentRejected(t *testing.T, ctx context.Context, registry revisionFencedDirectoryAssignment, siteID string) {
	t.Helper()
	before, err := registry.GetDescriptorByIDContext(ctx, siteID)
	if err != nil {
		t.Fatal("read assignment before CAS regression:", err)
	}
	first, err := registry.SetRuntimeCellIDAtRevisionContext(ctx, siteID, "deployment-cas-first", before.ConfigRevision)
	if err != nil {
		t.Fatal("apply first revision-fenced assignment:", err)
	}
	if _, err := registry.SetRuntimeCellIDAtRevisionContext(ctx, siteID, "deployment-cas-stale", before.ConfigRevision); !errors.Is(err, site.ErrSiteRevisionConflict) {
		t.Fatalf("stale assignment error = %v, want ErrSiteRevisionConflict", err)
	}
	current, err := registry.GetDescriptorByIDContext(ctx, siteID)
	if err != nil {
		t.Fatal("read assignment after stale CAS:", err)
	}
	if current.RuntimeCellID != first.RuntimeCellID || current.ConfigRevision != first.ConfigRevision {
		t.Fatalf("stale assignment overwrote current state: first=%+v current=%+v", first, current)
	}
}

func assertConcurrentDirectoryAssignmentConflict(t *testing.T, ctx context.Context, registry revisionFencedDirectoryAssignment, siteID string) {
	t.Helper()
	before, err := registry.GetDescriptorByIDContext(ctx, siteID)
	if err != nil {
		t.Fatal("read assignment before concurrent CAS regression:", err)
	}
	type result struct {
		descriptor site.Descriptor
		err        error
	}
	targets := []string{"deployment-cas-concurrent-a", "deployment-cas-concurrent-b"}
	start := make(chan struct{})
	results := make(chan result, len(targets))
	var group sync.WaitGroup
	group.Add(len(targets))
	for _, target := range targets {
		target := target
		go func() {
			defer group.Done()
			<-start
			descriptor, err := registry.SetRuntimeCellIDAtRevisionContext(ctx, siteID, target, before.ConfigRevision)
			results <- result{descriptor: descriptor, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)
	var winner *site.Descriptor
	conflicts := 0
	for result := range results {
		switch {
		case result.err == nil:
			if winner != nil {
				t.Fatalf("both concurrent assignments committed: %+v and %+v", *winner, result.descriptor)
			}
			descriptor := result.descriptor
			winner = &descriptor
		case errors.Is(result.err, site.ErrSiteRevisionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent assignment failed without a revision conflict: %v", result.err)
		}
	}
	if winner == nil || conflicts != 1 {
		t.Fatalf("concurrent assignment outcomes: winner=%+v conflicts=%d; want exactly one winner and one conflict", winner, conflicts)
	}
	current, err := registry.GetDescriptorByIDContext(ctx, siteID)
	if err != nil {
		t.Fatal("read assignment after concurrent CAS:", err)
	}
	if current.RuntimeCellID != winner.RuntimeCellID || current.ConfigRevision != before.ConfigRevision+1 {
		t.Fatalf("concurrent assignment state = %+v, winner=%+v, original revision=%d", current, winner, before.ConfigRevision)
	}
}
