package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/site"
	_ "github.com/go-sql-driver/mysql"
)

// TestLiveEngineCellsMoveAndRecover runs two real Engine processes against a
// tmpfs-backed MySQL directory and tenant database. It verifies default-cell
// forwarding, revisioned reassignment/hot-add, forwarding after a cell outage,
// and recovery after both cells restart. Existing local databases are never
// used by this opt-in acceptance test.
func TestLiveEngineCellsMoveAndRecover(t *testing.T) {
	if os.Getenv("KORA_SITE_DIRECTORY_LIVE_ENGINE_CELLS") != "1" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_ENGINE_CELLS=1 to run two-cell live acceptance")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for the disposable MySQL container")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	container := "kora-engine-cells-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	startDB := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", container,
		"--tmpfs", "/var/lib/mysql:rw,size=512m", "-e", "MYSQL_ROOT_PASSWORD=acceptance-only",
		"-e", "MYSQL_DATABASE=kora_platform", "-p", "127.0.0.1::3306", "mysql:8.0")
	if output, err := startDB.CombinedOutput(); err != nil {
		t.Fatalf("start disposable Engine-cell MySQL: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", container).Run() }()
	platformPort := inspectDockerPort(ctx, t, container, "3306/tcp")
	waitMySQLContainer(ctx, t, container)
	address := netJoinLoopback(platformPort)
	dsn := fmt.Sprintf("root:acceptance-only@tcp(%s)/kora_platform?parseTime=true&multiStatements=true", address)
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
	if err := site.BootstrapPlatformRegistry(database, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap disposable platform registry:", err)
	}
	tenantDBName := "kora_cell_tenant_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := database.ExecContext(ctx, "CREATE DATABASE `"+tenantDBName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create disposable tenant database:", err)
	}
	defer func() {
		if _, err := database.Exec("DROP DATABASE `" + tenantDBName + "`"); err != nil {
			t.Errorf("drop disposable tenant database: %v", err)
			return
		}
		var found string
		if err := database.QueryRow(`SELECT schema_name FROM information_schema.schemata WHERE schema_name = ?`, tenantDBName).Scan(&found); err != sql.ErrNoRows {
			t.Errorf("disposable tenant schema remains after cleanup: %q (%v)", found, err)
		}
	}()
	registry := site.NewSQLSiteRegistry(database, "mysql")
	hostname := "cell-move-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".example.invalid"
	if err := registry.Upsert(&site.SiteConfig{
		Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql", DBHost: "127.0.0.1",
		DBPort: mustPort(t, platformPort), DBName: tenantDBName, DBUser: "root", DBPassword: "acceptance-only",
	}); err != nil {
		t.Fatal("create disposable directory entry:", err)
	}
	info, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve disposable directory entry:", err)
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve Engine source directory")
	}
	root := filepath.Dir(filepath.Dir(sourceFile))
	binary := filepath.Join(t.TempDir(), "kora-engine-cell-acceptance")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	build.Dir = root
	build.Stdout, build.Stderr = io.Discard, io.Discard
	if err := build.Run(); err != nil {
		t.Fatal("build Engine for two-cell acceptance:", err)
	}
	defaultPort, cellAPort := freeTCPPort(t), freeTCPPort(t)
	for cellAPort == defaultPort {
		cellAPort = freeTCPPort(t)
	}
	configDir := t.TempDir()
	consumerSuffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	defaultEnv := isolatedCellEngineEnvironment(dsn, platformPort, configDir,
		"engine:acceptance-default:"+consumerSuffix, "default", "cell-a=http://127.0.0.1:"+strconv.Itoa(cellAPort))
	cellAEnv := isolatedCellEngineEnvironment(dsn, platformPort, configDir,
		"engine:acceptance-cell-a:"+consumerSuffix, "cell-a", "default=http://127.0.0.1:"+strconv.Itoa(defaultPort))
	var processes []*cellAcceptanceProcess
	defer func() {
		for index := len(processes) - 1; index >= 0; index-- {
			processes[index].stop(t)
		}
	}()
	defaultCell := startCellAcceptanceEngine(t, ctx, root, binary, defaultPort, defaultEnv)
	processes = append(processes, defaultCell)
	cellA := startCellAcceptanceEngine(t, ctx, root, binary, cellAPort, cellAEnv)
	processes = append(processes, cellA)
	client := &http.Client{Timeout: 2 * time.Second}
	defaultCell.waitPing(t, ctx, client)
	waitCellListener(t, ctx, cellAPort)

	// Before the move, cell-a must forward this site to the default Engine.
	if status := sitePathPing(client, cellAPort, info.SiteID); status != http.StatusOK {
		t.Fatalf("initial default-cell path through cell-a = HTTP %d, want 200", status)
	}

	assignment, err := registry.SetRuntimeCellID(info.SiteID, "cell-a")
	if err != nil {
		t.Fatal("move disposable site to cell-a:", err)
	}
	if assignment.ConfigRevision != info.ConfigRevision+1 {
		t.Fatalf("assignment revision = %d, want %d", assignment.ConfigRevision, info.ConfigRevision+1)
	}

	// Take the source cell down. Once the destination applied the outbox event
	// and eagerly loaded the tenant, its path must keep working without source.
	defaultCell.stop(t)
	waitForCellPath(t, ctx, client, cellAPort, info.SiteID, http.StatusOK)

	// Restart the old source with the same durable cursor. It should now proxy
	// the site to cell-a instead of loading the tenant locally.
	defaultCell = startCellAcceptanceEngine(t, ctx, root, binary, defaultPort, defaultEnv)
	processes = append(processes, defaultCell)
	waitCellListener(t, ctx, defaultPort)
	if status := sitePathPing(client, defaultPort, info.SiteID); status != http.StatusOK {
		t.Fatalf("default ingress after reassignment = HTTP %d, want 200", status)
	}

	// A destination outage is visible as 502 at shared ingress, and the same
	// route becomes healthy again when the assigned cell restarts and warms up.
	cellA.stop(t)
	waitForCellPath(t, ctx, client, defaultPort, info.SiteID, http.StatusBadGateway)
	cellA = startCellAcceptanceEngine(t, ctx, root, binary, cellAPort, cellAEnv)
	processes = append(processes, cellA)
	waitCellListener(t, ctx, cellAPort)
	waitForCellPath(t, ctx, client, defaultPort, info.SiteID, http.StatusOK)

	current, err := registry.GetDescriptorByID(info.SiteID)
	if err != nil {
		t.Fatal("read final disposable placement:", err)
	}
	if current.RuntimeCellID != "cell-a" || current.ConfigRevision != assignment.ConfigRevision {
		t.Fatalf("final placement = %+v; want cell-a at revision %d", current, assignment.ConfigRevision)
	}
	t.Logf("two Engine cells moved and recovered disposable site %s at revision %d", info.SiteID, current.ConfigRevision)
}

type cellAcceptanceProcess struct {
	cmd    *exec.Cmd
	done   chan struct{}
	err    error
	log    *os.File
	logDir string
	port   int
}

func startCellAcceptanceEngine(t *testing.T, ctx context.Context, root, binary string, port int, env []string) *cellAcceptanceProcess {
	t.Helper()
	dir := t.TempDir()
	logFile, err := os.Create(filepath.Join(dir, "engine.log"))
	if err != nil {
		t.Fatal("create cell Engine log:", err)
	}
	cmd := exec.CommandContext(ctx, binary, "serve", "--port", strconv.Itoa(port))
	cmd.Dir, cmd.Env = root, env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal("start cell Engine:", err)
	}
	process := &cellAcceptanceProcess{cmd: cmd, done: make(chan struct{}), log: logFile, logDir: dir, port: port}
	go func() {
		process.err = cmd.Wait()
		close(process.done)
	}()
	return process
}

func (p *cellAcceptanceProcess) waitPing(t *testing.T, ctx context.Context, client *http.Client) {
	t.Helper()
	for {
		select {
		case <-p.done:
			t.Fatalf("cell Engine on port %d exited before ping (err=%v; logs=%s)", p.port, p.err, redactedCellEngineLogs(p))
		default:
		}
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/ping", p.port))
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("cell Engine on port %d did not become ready: %v; logs=%s", p.port, ctx.Err(), redactedCellEngineLogs(p))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (p *cellAcceptanceProcess) stop(t *testing.T) {
	t.Helper()
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		_ = p.log.Close()
		return
	default:
	}
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Errorf("signal cell Engine on port %d: %v", p.port, err)
	}
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Errorf("cell Engine on port %d did not stop gracefully; logs=%s", p.port, redactedCellEngineLogs(p))
	}
	_ = p.log.Close()
}

func sitePathPing(client *http.Client, port int, siteID string) int {
	target := fmt.Sprintf("http://127.0.0.1:%d/s/%s/api/v1/ping?cell_acceptance=1", port, url.PathEscape(siteID))
	response, err := client.Get(target)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func waitForCellPath(t *testing.T, ctx context.Context, client *http.Client, port int, siteID string, want int) {
	t.Helper()
	for {
		if got := sitePathPing(client, port, siteID); got == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("site route through port %d did not become HTTP %d: %v", port, want, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitCellListener(t *testing.T, ctx context.Context, port int) {
	t.Helper()
	waitTCP(ctx, t, fmt.Sprintf("127.0.0.1:%d", port))
}

func isolatedCellEngineEnvironment(dsn, platformPort, configDir, consumerID, cellID, cellURLs string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true}
	filtered := make([]string, 0, len(allowed)+12)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered,
		"DB_DSN="+dsn,
		"KORA_DB_TYPE=mysql",
		"KORA_DB_HOST=127.0.0.1",
		"KORA_DB_PORT="+platformPort,
		"KORA_DB_USER=root",
		"KORA_DB_PASSWORD=acceptance-only",
		"KORA_CONFIG_DIR="+configDir,
		"KORA_SITE_DIRECTORY_CONSUMER_ID="+consumerID,
		"KORA_ENGINE_CELL_ID="+cellID,
		"KORA_ENGINE_CELL_URLS="+cellURLs,
		"KORA_DB_MAX_OPEN=2",
		"KORA_DB_MAX_IDLE=1",
		"KORA_SCRIPTS_ENABLED=false",
		"KORA_LOG_LEVEL=warn",
		"KORA_LOG_FORMAT=json",
	)
}

func redactedCellEngineLogs(p *cellAcceptanceProcess) string {
	_ = p.log.Sync()
	data, _ := os.ReadFile(filepath.Join(p.logDir, "engine.log"))
	return strings.TrimSpace(strings.ReplaceAll(string(data), "acceptance-only", "[redacted]"))
}

func netJoinLoopback(port string) string {
	return "127.0.0.1:" + port
}
