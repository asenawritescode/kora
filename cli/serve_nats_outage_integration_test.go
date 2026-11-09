package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
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
	_ "github.com/lib/pq"
)

// TestLiveEngineProcessResilienceToNATSAndPlatformDatabaseOutages exercises
// the actual Engine serve process against disposable tmpfs-backed MySQL
// platform, PostgreSQL tenant, and NATS containers. It is intentionally opt-in; set
// KORA_SITE_DIRECTORY_LIVE_ENGINE_NATS_OUTAGE=1.
func TestLiveEngineProcessResilienceToNATSAndPlatformDatabaseOutages(t *testing.T) {
	if os.Getenv("KORA_SITE_DIRECTORY_LIVE_ENGINE_NATS_OUTAGE") != "1" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_ENGINE_NATS_OUTAGE=1 to run the live Engine/NATS outage acceptance")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required for disposable MySQL and NATS processes")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	mysqlContainer := "kora-engine-outage-mysql-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	startMySQL := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", mysqlContainer,
		"--tmpfs", "/var/lib/mysql:rw,size=512m", "-e", "MYSQL_ROOT_PASSWORD=acceptance-only",
		"-e", "MYSQL_DATABASE=kora_platform", "-p", "127.0.0.1::3306", "mysql:8.0")
	if output, err := startMySQL.CombinedOutput(); err != nil {
		t.Fatalf("start disposable platform/tenant MySQL: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", mysqlContainer).Run() }()
	platformPort := inspectDockerPort(ctx, t, mysqlContainer, "3306/tcp")
	waitMySQLContainer(ctx, t, mysqlContainer)
	tenantContainer := "kora-engine-outage-tenant-pg-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	startTenantDB := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", tenantContainer,
		"--tmpfs", "/var/lib/postgresql/data:rw,size=512m", "-e", "POSTGRES_PASSWORD=acceptance-only",
		"-e", "POSTGRES_DB=kora_tenant", "-p", "127.0.0.1::5432", "postgres:16")
	if output, err := startTenantDB.CombinedOutput(); err != nil {
		t.Fatalf("start disposable PostgreSQL tenant database: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", tenantContainer).Run() }()
	tenantPort := inspectDockerPort(ctx, t, tenantContainer, "5432/tcp")
	waitPostgresContainer(ctx, t, tenantContainer)
	platformAddress := net.JoinHostPort("127.0.0.1", platformPort)
	tenantAddress := net.JoinHostPort("127.0.0.1", tenantPort)
	tenantDSN := fmt.Sprintf("postgres://postgres:acceptance-only@%s/kora_tenant?sslmode=disable", tenantAddress)
	tenantDB, err := sql.Open("postgres", tenantDSN)
	if err != nil {
		t.Fatal("open disposable tenant database:", err)
	}
	defer tenantDB.Close()
	for {
		if err := tenantDB.PingContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disposable PostgreSQL tenant database did not accept host connections:", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	dsn := fmt.Sprintf("root:acceptance-only@tcp(%s)/kora_platform?parseTime=true&multiStatements=true", platformAddress)
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL platform database:", err)
	}
	defer database.Close()
	for {
		if err := database.PingContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disposable MySQL did not become ready:", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err := site.BootstrapPlatformRegistry(database, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap disposable platform registry:", err)
	}
	registry := site.NewSQLSiteRegistry(database, "mysql")
	hostname := fmt.Sprintf("engine-nats-outage-%d.example.invalid", time.Now().UnixNano())
	if err := registry.Upsert(&site.SiteConfig{
		Hostname: hostname, DomainsList: []string{hostname}, DBType: "postgres", DBHost: "127.0.0.1",
		DBPort: mustPort(t, tenantPort), DBName: "kora_tenant", DBUser: "postgres", DBPassword: "acceptance-only",
	}); err != nil {
		t.Fatal("create disposable Engine site:", err)
	}
	info, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve disposable Engine site:", err)
	}

	container := "kora-engine-outage-nats-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	startNATS := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", container, "-p", "127.0.0.1::4222", "nats:2.10", "-js")
	if output, err := startNATS.CombinedOutput(); err != nil {
		t.Fatalf("start disposable NATS container: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	defer func() {
		stop := exec.Command("docker", "rm", "-f", container)
		_ = stop.Run()
	}()
	inspect := exec.CommandContext(ctx, "docker", "inspect", "-f", `{{(index (index .NetworkSettings.Ports "4222/tcp") 0).HostPort}}`, container)
	portBytes, err := inspect.Output()
	if err != nil {
		t.Fatal("resolve disposable NATS port:", err)
	}
	natsPort := strings.TrimSpace(string(portBytes))
	if natsPort == "" {
		t.Fatal("disposable NATS container has no published client port")
	}
	natsAddress := net.JoinHostPort("127.0.0.1", natsPort)
	waitTCP(ctx, t, natsAddress)

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source directory")
	}
	root := filepath.Dir(filepath.Dir(sourceFile))
	binary := filepath.Join(t.TempDir(), "kora-engine-outage-test")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	build.Dir = root
	build.Stdout = io.Discard
	build.Stderr = io.Discard
	if err := build.Run(); err != nil {
		t.Fatal("build Engine process for live acceptance:", err)
	}
	port := freeTCPPort(t)
	consumerID := "integration:engine-nats-outage:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	process := exec.Command(binary, "serve", "--port", strconv.Itoa(port))
	process.Dir = root
	process.Env = isolatedEngineEnvironment(dsn, natsAddress, consumerID, platformPort, t.TempDir())
	logFile, err := os.Create(filepath.Join(t.TempDir(), "engine.log"))
	if err != nil {
		t.Fatal("create Engine log:", err)
	}
	process.Stdout = logFile
	process.Stderr = logFile
	if err := process.Start(); err != nil {
		logFile.Close()
		t.Fatal("start Engine process:", err)
	}
	engineDone := make(chan struct{})
	var engineExitErr error
	go func() {
		engineExitErr = process.Wait()
		close(engineDone)
	}()
	stopped := false
	defer func() {
		if !stopped && process.Process != nil {
			_ = process.Process.Signal(os.Interrupt)
			select {
			case <-engineDone:
			case <-time.After(10 * time.Second):
				_ = process.Process.Kill()
				<-engineDone
			}
		}
		_ = logFile.Close()
	}()

	client := &http.Client{Timeout: time.Second}
	waitEnginePing(ctx, t, client, port, hostname, engineDone, func() error {
		_ = logFile.Sync()
		logs, _ := os.ReadFile(logFile.Name())
		return fmt.Errorf("%v\nengine log:\n%s", engineExitErr, strings.TrimSpace(string(logs)))
	})

	// Engine no longer exposes password-based self-service tenant bootstrap;
	// Cloud owns signup and provisions a passwordless Cloud-linked principal.
	retired, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/api/console/sites/onboard", port), "application/json", strings.NewReader(`{"hostname":"retired.example.invalid","admin_email":"owner@example.invalid","admin_password":"no-longer-supported"}`))
	if err != nil {
		t.Fatal("request retired Engine password bootstrap route:", err)
	}
	retired.Body.Close()
	if retired.StatusCode != http.StatusNotFound {
		t.Fatalf("retired Engine password bootstrap returned HTTP %d, want 404", retired.StatusCode)
	}

	if _, err := registry.CurrentDirectoryRevision(); err != nil {
		t.Fatal("read current directory revision:", err)
	}
	// Exercise the directory consumer's complete runtime lifecycle while the
	// real Engine process is serving: hot-add a site, then tombstone it.
	hotAddedHost := fmt.Sprintf("hot-added-%d.example.invalid", time.Now().UnixNano())
	if err := registry.Upsert(&site.SiteConfig{
		Hostname: hotAddedHost, DomainsList: []string{hotAddedHost}, DBType: "postgres", DBHost: "127.0.0.1",
		DBPort: mustPort(t, tenantPort), DBName: "kora_tenant", DBUser: "postgres", DBPassword: "acceptance-only",
	}); err != nil {
		t.Fatal("commit hot-add site registration:", err)
	}
	hotAdded, err := registry.ResolveAlias(hotAddedHost)
	if err != nil {
		t.Fatal("resolve hot-added site:", err)
	}
	latest, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read hot-add directory revision:", err)
	}
	waitForConsumerCursor(ctx, t, database, consumerID, latest)
	if status := enginePing(client, port, hotAddedHost); status != http.StatusOK {
		t.Fatalf("hot-added site did not become routable after durable apply: HTTP %d", status)
	}
	if err := registry.SetStatus(hotAdded.SiteID, "deleted"); err != nil {
		t.Fatal("tombstone hot-added site:", err)
	}
	latest, err = registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read tombstone directory revision:", err)
	}
	waitForConsumerCursor(ctx, t, database, consumerID, latest)
	if status := enginePing(client, port, hotAddedHost); status != http.StatusNotFound {
		t.Fatalf("tombstoned site remained routable: HTTP %d", status)
	}
	stopNATS := exec.CommandContext(ctx, "docker", "stop", container)
	if output, err := stopNATS.CombinedOutput(); err != nil {
		t.Fatalf("stop disposable NATS process: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	waitTCPClosed(ctx, t, natsAddress)
	if status := enginePing(client, port, hostname); status != http.StatusOK {
		t.Fatalf("Engine stopped serving after NATS process loss: HTTP %d", status)
	}

	newAlias := "recovered." + hostname
	if err := site.UpdatePlatformSiteRegistration(database, "mysql", hostname, []string{hostname, newAlias}, "local", ""); err != nil {
		t.Fatal("commit directory update during NATS outage:", err)
	}
	latest, err = registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read updated directory revision:", err)
	}
	waitForConsumerCursor(ctx, t, database, consumerID, latest)
	if status := enginePing(client, port, newAlias); status != http.StatusOK {
		t.Fatalf("Engine did not route newly committed alias while NATS was down: HTTP %d", status)
	}
	if status := enginePing(client, port, hostname); status != http.StatusOK {
		t.Fatalf("Engine stopped serving existing alias during NATS outage: HTTP %d", status)
	}

	// The platform directory may be unavailable while already-loaded tenant
	// routes continue serving from the last known-good local snapshot.
	pausePlatform := exec.CommandContext(ctx, "docker", "pause", mysqlContainer)
	if output, err := pausePlatform.CombinedOutput(); err != nil {
		t.Fatalf("pause disposable platform database: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	platformPaused := true
	defer func() {
		if platformPaused {
			_ = exec.Command("docker", "unpause", mysqlContainer).Run()
		}
	}()
	for _, alias := range []string{hostname, newAlias} {
		if status := enginePing(client, port, alias); status != http.StatusOK {
			t.Fatalf("Engine stopped serving cached alias %s during platform DB outage: HTTP %d", alias, status)
		}
	}
	time.Sleep(2 * time.Second)
	for _, alias := range []string{hostname, newAlias} {
		if status := enginePing(client, port, alias); status != http.StatusOK {
			t.Fatalf("Engine stopped serving cached alias %s during sustained platform DB outage: HTTP %d", alias, status)
		}
	}
	unpausePlatform := exec.CommandContext(ctx, "docker", "unpause", mysqlContainer)
	if output, err := unpausePlatform.CombinedOutput(); err != nil {
		t.Fatalf("resume disposable platform database: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	platformPaused = false
	for {
		if err := database.PingContext(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("platform database did not recover:", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	finalAlias := "recovered-again." + hostname
	if err := site.UpdatePlatformSiteRegistration(database, "mysql", hostname, []string{hostname, newAlias, finalAlias}, "local", ""); err != nil {
		t.Fatal("commit directory update after platform DB recovery:", err)
	}
	latest, err = registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read recovered directory revision:", err)
	}
	waitForConsumerCursor(ctx, t, database, consumerID, latest)
	if status := enginePing(client, port, finalAlias); status != http.StatusOK {
		t.Fatalf("Engine did not resume directory propagation after platform DB recovery: HTTP %d", status)
	}

	_ = process.Process.Signal(os.Interrupt)
	<-engineDone
	if engineExitErr != nil {
		t.Fatalf("Engine process shutdown: %v", engineExitErr)
	}
	stopped = true
	_ = logFile.Close()
	if err := registry.SetStatus(info.SiteID, "deleted"); err != nil {
		t.Errorf("tombstone disposable test site: %v", err)
	}
}

func isolatedEngineEnvironment(dsn, natsAddress, consumerID, platformPort, configDir string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true}
	filtered := make([]string, 0, len(allowed)+8)
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
		"KORA_CONSOLE_ONBOARDING_ENABLED=true",
		"CONSOLE_EMAIL=acceptance@example.invalid",
		"CONSOLE_PASSWORD=acceptance-only-console-password",
		"KORA_EVENT_PROVIDER=nats",
		"KORA_NATS_URLS=nats://"+natsAddress,
		"KORA_SITE_DIRECTORY_CONSUMER_ID="+consumerID,
		"KORA_SCRIPTS_ENABLED=false",
	)
}

func inspectDockerPort(ctx context.Context, t *testing.T, container, port string) string {
	t.Helper()
	format := fmt.Sprintf(`{{(index (index .NetworkSettings.Ports %q) 0).HostPort}}`, port)
	inspect := exec.CommandContext(ctx, "docker", "inspect", "-f", format, container)
	bytes, err := inspect.Output()
	if err != nil {
		t.Fatalf("resolve published port for disposable container: %v", err)
	}
	value := strings.TrimSpace(string(bytes))
	if value == "" {
		t.Fatalf("disposable container %s has no published port %s", container, port)
	}
	return value
}

func waitMySQLContainer(ctx context.Context, t *testing.T, container string) {
	t.Helper()
	for {
		ping := exec.CommandContext(ctx, "docker", "exec", container, "mysqladmin", "ping", "-uroot", "-pacceptance-only", "--silent")
		if ping.Run() == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("disposable MySQL %s did not become ready: %v", container, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func waitPostgresContainer(ctx context.Context, t *testing.T, container string) {
	t.Helper()
	for {
		ready := exec.CommandContext(ctx, "docker", "exec", container, "pg_isready", "-U", "postgres", "-d", "kora_tenant")
		if ready.Run() == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("disposable PostgreSQL %s did not become ready: %v", container, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("reserve Engine port:", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func mustPort(t *testing.T, value string) int {
	t.Helper()
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("invalid disposable port %q: %v", value, err)
	}
	return port
}

func waitTCP(ctx context.Context, t *testing.T, address string) {
	t.Helper()
	for {
		conn, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", address, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitTCPClosed(ctx context.Context, t *testing.T, address string) {
	t.Helper()
	for {
		conn, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		select {
		case <-ctx.Done():
			t.Fatalf("NATS still accepts connections at %s after container stop", address)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func enginePing(client *http.Client, port int, hostname string) int {
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/v1/ping", port), nil)
	if err != nil {
		return 0
	}
	request.Host = hostname
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	return response.StatusCode
}

func waitEnginePing(ctx context.Context, t *testing.T, client *http.Client, port int, hostname string, engineDone <-chan struct{}, engineExitErr func() error) {
	t.Helper()
	for {
		if status := enginePing(client, port, hostname); status == http.StatusOK {
			return
		}
		select {
		case <-engineDone:
			t.Fatalf("Engine process exited before becoming ready: %v", engineExitErr())
		case <-ctx.Done():
			t.Fatalf("Engine did not become ready on port %d: %v", port, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func waitForConsumerCursor(ctx context.Context, t *testing.T, database *sql.DB, consumerID string, revision uint64) {
	t.Helper()
	for {
		var current uint64
		err := database.QueryRowContext(ctx, `SELECT directory_revision FROM _kora_site_directory_consumers WHERE consumer_id = ?`, consumerID).Scan(&current)
		if err == nil && current >= revision {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Engine SQL consumer cursor did not reach revision %d (last=%d, err=%v)", revision, current, err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}
