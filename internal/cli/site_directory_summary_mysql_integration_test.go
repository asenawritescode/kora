//go:build integration

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/site"
	_ "github.com/go-sql-driver/mysql"
)

// TestLiveMySQLSiteDirectoryAssignmentSummary exercises the operator command
// through real MySQL directory paging and revision reads. The database is
// created in a fresh tmpfs-backed container, and only its platform registry is
// written; tenant databases are never opened.
func TestLiveMySQLSiteDirectoryAssignmentSummary(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for the disposable MySQL summary acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	container := fmt.Sprintf("kora-directory-summary-%d", time.Now().UnixNano())
	start := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", container,
		"--tmpfs", "/var/lib/mysql:rw,size=768m", "-e", "MYSQL_ROOT_PASSWORD=summary-acceptance-only",
		"-e", "MYSQL_DATABASE=kora_platform", "-p", "127.0.0.1::3306", "mysql:8.4.11")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start disposable assignment-summary MySQL: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", container).Run() }()
	port := inspectDockerPort(ctx, t, container, "3306/tcp")
	waitMySQLContainer(ctx, t, container)
	dsn := fmt.Sprintf("root:summary-acceptance-only@tcp(%s)/kora_platform?parseTime=true&multiStatements=true", netJoinLoopback(port))
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable platform registry:", err)
	}
	defer database.Close()
	for {
		if err := database.PingContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disposable platform registry did not accept host connections:", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err := site.BootstrapPlatformRegistry(context.Background(), database, kdb.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap disposable platform registry:", err)
	}
	registry := site.NewSQLSiteRegistry(database, "mysql")
	const entries = 502
	var tenantIDs []string
	want := make(map[directoryAssignmentKey]uint64)
	for i := 0; i < entries; i++ {
		hostname := fmt.Sprintf("directory-summary-%03d.example.invalid", i)
		if err := registry.Upsert(&site.Config{
			Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql",
			DBHost: "127.0.0.1", DBPort: 3306, DBName: "never-opened-tenant-db",
		}); err != nil {
			t.Fatalf("insert disposable directory row %d: %v", i, err)
		}
		info, err := registry.ResolveAlias(hostname)
		if err != nil {
			t.Fatalf("resolve disposable directory row %d: %v", i, err)
		}
		tenantIDs = append(tenantIDs, info.SiteID)
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
				t.Fatalf("assign disposable directory row %d: %v", i, err)
			}
		}
		status := "active"
		if i%2 == 1 {
			status = "provisioning"
			if _, err := registry.SetStatusContext(ctx, info.SiteID, status); err != nil {
				t.Fatalf("set disposable directory status %d: %v", i, err)
			}
		}
		placement, err = site.NormalizeRuntimeCellID(placement)
		if err != nil {
			t.Fatal("normalize expected placement:", err)
		}
		if placement == "" {
			placement = "default"
		}
		want[directoryAssignmentKey{deploymentID: placement, status: status}]++
	}

	t.Setenv("KORA_DB_TYPE", "mysql")
	t.Setenv("DB_DSN", dsn)
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetContext(ctx)
	if err := runSiteDirectoryAssignmentSummary(command, nil); err != nil {
		t.Fatal("run SQL-backed assignment summary command:", err)
	}
	var got struct {
		Version           int                         `json:"version"`
		PlacementSource   string                      `json:"placement_source"`
		DirectoryRevision uint64                      `json:"directory_revision"`
		TotalEntries      uint64                      `json:"total_entries"`
		Assignments       []directoryAssignmentBucket `json:"assignments"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal("decode aggregate summary output:", err)
	}
	if got.Version != 1 || got.PlacementSource != "runtime_cell_id" || got.DirectoryRevision == 0 || got.TotalEntries != entries {
		t.Fatalf("summary metadata = %+v, want version 1, v1 source, revision, and %d rows", got, entries)
	}
	if len(got.Assignments) != len(want) {
		t.Fatalf("summary buckets = %+v, want %d buckets", got.Assignments, len(want))
	}
	for _, bucket := range got.Assignments {
		key := directoryAssignmentKey{deploymentID: bucket.DeploymentID, status: bucket.Status}
		if bucket.Count != want[key] {
			t.Fatalf("summary bucket %+v count=%d, want %d", key, bucket.Count, want[key])
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("summary omitted expected buckets: %+v", want)
	}
	for _, private := range tenantIDs {
		if strings.Contains(output.String(), private) {
			t.Fatalf("aggregate output leaked tenant identity %q", private)
		}
	}
	if strings.Contains(output.String(), "directory-summary-") || strings.Contains(output.String(), "never-opened-tenant-db") {
		t.Fatalf("aggregate output leaked aliases or tenant connection metadata: %s", output.String())
	}
	assertStaleDirectoryAssignmentRejected(t, ctx, registry, tenantIDs[0])
	assertConcurrentDirectoryAssignmentConflict(t, ctx, registry, tenantIDs[1])
}
