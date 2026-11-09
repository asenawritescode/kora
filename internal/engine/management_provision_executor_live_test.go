//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/management"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

type liveManagementProvisionCoordinator struct{ activated atomic.Int32 }

func (c *liveManagementProvisionCoordinator) ActivateFromDirectory(context.Context, string) error {
	c.activated.Add(1)
	return nil
}
func (*liveManagementProvisionCoordinator) Retire(context.Context, string) error { return nil }

func TestLiveManagementProvisionReplay(t *testing.T) {
	tests := []struct {
		name, driver, env string
	}{
		{name: "mysql", driver: "mysql", env: "KORA_MANAGEMENT_PROVISION_DISPOSABLE_MYSQL_DSN"},
		{name: "postgres", driver: "postgres", env: "KORA_MANAGEMENT_PROVISION_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { testLiveManagementProvisionReplay(t, test.driver, os.Getenv(test.env)) })
	}
}

// TestLiveManagementProvisionRouteAndWorker exercises the production Engine
// management route, SQL operation store, fenced worker, and real tenant creator
// together. The first response is intentionally dropped after durable acceptance;
// retrying the exact command must return one stable operation identity.
func TestLiveManagementProvisionRouteAndWorker(t *testing.T) {
	for _, test := range []struct{ name, driver, env string }{
		{"mysql", "mysql", "KORA_MANAGEMENT_PROVISION_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_MANAGEMENT_PROVISION_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable loopback SQL service", test.env)
			}
			testLiveManagementProvisionRouteAndWorker(t, test.driver, dsn)
		})
	}
}

func testLiveManagementProvisionRouteAndWorker(t *testing.T, driver, dsn string) {
	t.Helper()
	platformDB, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open explicitly disposable management database:", err)
	}
	defer platformDB.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := platformDB.PingContext(ctx); err != nil {
		t.Fatal("ping explicitly disposable management database:", err)
	}
	dialect := db.Resolve(driver)
	if err := site.BootstrapPlatformRegistryContext(ctx, platformDB, dialect); err != nil {
		t.Fatal("bootstrap platform registry:", err)
	}
	host, port, user, password, err := liveDatabaseTarget(driver, dsn)
	if err != nil {
		t.Fatal("read database connection fields:", err)
	}
	stamp := time.Now().UnixNano()
	command := contract.ProvisionTenant{
		Meta:                  contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("route-provision-" + strconv.FormatInt(stamp, 10))},
		Tenant:                contract.TenantID(fmt.Sprintf("route-tenant-%s-%d", driver, stamp)),
		BootstrapAdminEmail:   fmt.Sprintf("route-admin-%d@example.invalid", stamp),
		BootstrapAdminSubject: fmt.Sprintf("usr_route_%d", stamp),
		PrimaryAlias:          fmt.Sprintf("route-%d.example.invalid", stamp),
		Aliases:               []string{fmt.Sprintf("route-alias-%d.example.invalid", stamp)},
		Deployment:            "deployment-live", DatabaseAllocationRef: driver + ":route-disposable-allocation",
		DisplayName: "Dbg Duka",
	}
	if err := contract.ValidateProvisionTenant(command); err != nil {
		t.Fatal("validate command:", err)
	}
	name := strings.ReplaceAll(command.PrimaryAlias, ".", "_")
	registry := site.NewSQLSiteRegistry(platformDB, driver)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = platformDB.ExecContext(cleanupCtx, db.Rebind(dialect, `DELETE FROM _kora_site_alias WHERE site_id = ?`), string(command.Tenant))
		_, _ = platformDB.ExecContext(cleanupCtx, db.Rebind(dialect, `DELETE FROM _kora_site_registry WHERE site_id = ?`), string(command.Tenant))
		quoted := "`" + strings.ReplaceAll(name, "`", "``") + "`"
		if driver == "postgres" {
			quoted = `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		}
		_, _ = platformDB.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS "+quoted)
	})
	admission := &managementProvisionAdmissionStub{}
	coordinator := &liveManagementProvisionCoordinator{}
	const token, caller = "disposable-management-token", "cloud-control-plane"
	runtime, err := assembleManagementOperationRuntime(ctx, token, caller, platformDB, dialect, admission, registry, coordinator, knet.NewSiteRouter(nil), nil, nil, driver, &site.CommonConfig{DBType: driver, DBHost: host, DBPort: port, DBUser: user, DBPassword: password}, string(command.Deployment), command.DatabaseAllocationRef)
	if err != nil {
		t.Fatal("compose management runtime:", err)
	}
	if !runtime.routes.Capabilities[api.ManagementProvisionCapability] || !runtime.routes.Capabilities[api.ManagementProvisionRecovery] {
		t.Fatal("provision capabilities were not composed")
	}
	if runtime.routes.Capabilities[api.ManagementDeleteCapability] || runtime.routes.Capabilities[api.ManagementDeleteRecovery] {
		t.Fatal("tenant deletion was advertised without a trusted checkpoint verifier")
	}
	verifiedRuntime, err := assembleManagementOperationRuntime(ctx, token, caller, platformDB, dialect, admission, registry, coordinator, knet.NewSiteRouter(nil), nil, &managementCheckpointVerifierStub{}, driver, &site.CommonConfig{DBType: driver, DBHost: host, DBPort: port, DBUser: user, DBPassword: password}, string(command.Deployment), command.DatabaseAllocationRef)
	if err != nil {
		t.Fatal("compose management runtime with checkpoint verifier:", err)
	}
	if !verifiedRuntime.routes.Capabilities[api.ManagementDeleteCapability] || !verifiedRuntime.routes.Capabilities[api.ManagementDeleteRecovery] {
		t.Fatal("tenant deletion capabilities were not composed with a trusted checkpoint verifier")
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if err := RegisterPlatformRoutes(PlatformRouteConfig{Router: router, Directory: registry, ManagementToken: token, ManagementCallerID: caller, ManagementOperations: &runtime.routes, DeploymentID: string(command.Deployment), PlatformDB: platformDB, PlatformWorkAdmission: admission}); err != nil {
		t.Fatal("register Engine routes:", err)
	}
	var posts atomic.Int32
	var dropped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/management/v2/tenants" {
			posts.Add(1)
			if dropped.CompareAndSwap(false, true) {
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, r)
				if recorder.Code != http.StatusAccepted {
					w.WriteHeader(recorder.Code)
					_, _ = io.Copy(w, recorder.Body)
				}
				return
			}
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal("marshal command:", err)
	}
	post := func() (contract.ManagedOperation, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/management/v2/tenants", strings.NewReader(string(body)))
		if err != nil {
			return contract.ManagedOperation{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", string(command.Meta.IdempotencyKey))
		resp, err := server.Client().Do(req)
		if err != nil {
			return contract.ManagedOperation{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			data, _ := io.ReadAll(resp.Body)
			return contract.ManagedOperation{}, fmt.Errorf("provision route status %d: %s", resp.StatusCode, data)
		}
		var envelope struct {
			Data contract.ManagedOperation `json:"data"`
		}
		err = json.NewDecoder(resp.Body).Decode(&envelope)
		return envelope.Data, err
	}
	if _, err := post(); err == nil {
		t.Fatal("first accepted response was not lost")
	}
	accepted, err := post()
	if err != nil {
		t.Fatal("retry provision request:", err)
	}
	if accepted.ID == "" || accepted.Tenant != command.Tenant || accepted.State != contract.OperationRequested {
		t.Fatalf("retry response = %+v", accepted)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- runtime.worker.Run(workerCtx, runtime.config) }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			stopWorker()
			<-workerDone
			t.Fatal("timed out waiting for provisioned tenant")
		case <-ticker.C:
			info, lookupErr := registry.GetByIDContext(ctx, string(command.Tenant))
			if lookupErr == nil && info.Status == "active" {
				stopWorker()
				if err := <-workerDone; err != nil {
					t.Fatal("join worker:", err)
				}
				if posts.Load() != 2 || coordinator.activated.Load() != 1 {
					t.Fatalf("posts/activations = %d/%d, want 2/1", posts.Load(), coordinator.activated.Load())
				}
				var admins int
				tenantDB, err := site.ConnectContext(ctx, &site.Config{DBType: info.DBType, DBHost: info.DBHost, DBPort: info.DBPort, DBName: info.DBName, DBUser: info.DBUser, DBPassword: info.DBPassword})
				if err != nil {
					t.Fatal("connect provisioned tenant:", err)
				}
				err = tenantDB.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_user WHERE email = ?`), command.BootstrapAdminEmail).Scan(&admins)
				var deliveryRows int
				deliveryErr := tenantDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_magic_link_delivery`).Scan(&deliveryRows)
				var appName string
				appNameErr := tenantDB.QueryRowContext(ctx, db.Rebind(dialect, `SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?`), command.PrimaryAlias, "branding.app_name").Scan(&appName)
				closeErr := tenantDB.Close()
				if err != nil {
					t.Fatal("read bootstrap administrator:", err)
				}
				if appNameErr != nil || appName != command.DisplayName {
					t.Fatalf("provisioned branding.app_name = %q, %v; want %q", appName, appNameErr, command.DisplayName)
				}
				if closeErr != nil {
					t.Fatal("close tenant database:", closeErr)
				}
				if admins != 1 {
					t.Fatalf("bootstrap admin count = %d, want 1", admins)
				}
				if deliveryErr != nil {
					t.Fatalf("query invitation delivery journal after tenant bootstrap: %v", deliveryErr)
				}
				if deliveryRows != 0 {
					t.Fatalf("unexpected invitation delivery rows after tenant bootstrap = %d", deliveryRows)
				}
				return
			}
		}
	}
}

func testLiveManagementProvisionReplay(t *testing.T, driver, dsn string) {
	t.Helper()
	if strings.TrimSpace(dsn) == "" {
		t.Skip("set the dialect's KORA_MANAGEMENT_PROVISION_DISPOSABLE_*_DSN to a disposable database server")
	}
	platformDB, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open explicitly disposable management provisioning database:", err)
	}
	defer platformDB.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := platformDB.PingContext(ctx); err != nil {
		t.Fatal("ping explicitly disposable management provisioning database:", err)
	}
	if err := site.BootstrapPlatformRegistryContext(ctx, platformDB, db.Resolve(driver)); err != nil {
		t.Fatal("bootstrap platform site directory:", err)
	}
	host, port, user, password, err := liveDatabaseTarget(driver, dsn)
	if err != nil {
		t.Fatal("read database connection fields:", err)
	}
	stamp := time.Now().UnixNano()
	command := contract.ProvisionTenant{
		Meta:                  contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("live-provision-" + strconv.FormatInt(stamp, 10))},
		Tenant:                contract.TenantID(fmt.Sprintf("tenant-%s-%d", driver, stamp)),
		BootstrapAdminEmail:   fmt.Sprintf("admin-%d@example.invalid", stamp),
		BootstrapAdminSubject: fmt.Sprintf("usr_live_%d", stamp),
		PrimaryAlias:          fmt.Sprintf("mgmt-%d.example.invalid", stamp),
		Aliases:               []string{fmt.Sprintf("alias-%d.example.invalid", stamp)},
		Deployment:            "deployment-live",
		DatabaseAllocationRef: driver + ":disposable-allocation",
	}
	if err := contract.ValidateProvisionTenant(command); err != nil {
		t.Fatal("validate integration command:", err)
	}
	databaseName := strings.ReplaceAll(command.PrimaryAlias, ".", "_")
	if err := site.ValidateTenantDatabaseName(driver, databaseName); err != nil {
		t.Fatal("validate isolated tenant database name:", err)
	}
	registry := site.NewSQLSiteRegistry(platformDB, driver)
	cleanup := func() {
		_, _ = platformDB.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_site_alias WHERE site_id = ?`), string(command.Tenant))
		_, _ = platformDB.ExecContext(context.Background(), db.Rebind(db.Resolve(driver), `DELETE FROM _kora_site_registry WHERE site_id = ?`), string(command.Tenant))
		quoted := "`" + strings.ReplaceAll(databaseName, "`", "``") + "`"
		if driver == "postgres" {
			quoted = `"` + strings.ReplaceAll(databaseName, `"`, `""`) + `"`
		}
		_, _ = platformDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoted)
	}
	t.Cleanup(cleanup)

	coordinator := &liveManagementProvisionCoordinator{}
	common := &site.CommonConfig{DBType: driver, DBHost: host, DBPort: port, DBUser: user, DBPassword: password}
	admission := &managementProvisionAdmissionStub{}
	executor, err := newManagementProvisionExecutor(command.DatabaseAllocationRef, string(command.Deployment), platformDB, driver, common, admission, registry, coordinator, site.CreateSiteContext)
	if err != nil {
		t.Fatal("configure live provisioning executor:", err)
	}
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal("encode provisioning command:", err)
	}
	operation := management.Operation{ID: "operation-live-1", Tenant: command.Tenant, Kind: management.CommandProvisionTenant, Command: payload}
	first, err := executor.Execute(ctx, managementTestClaim(operation))
	if err != nil || first.State != contract.OperationSucceeded {
		t.Fatalf("first provisioning execution = %+v, %v", first, err)
	}
	info, err := registry.GetByIDContext(ctx, string(command.Tenant))
	if err != nil {
		t.Fatal("read provisioned directory entry:", err)
	}
	if info.Status != "active" || info.Name != command.PrimaryAlias || !sameAliases(info.Domains, append([]string{command.PrimaryAlias}, command.Aliases...)) {
		t.Fatalf("provisioned directory entry = %+v", info)
	}
	tenantDB, err := site.ConnectContext(ctx, &site.Config{
		DBType: info.DBType, DBHost: info.DBHost, DBPort: info.DBPort, DBName: info.DBName,
		DBUser: info.DBUser, DBPassword: info.DBPassword,
	})
	if err != nil {
		t.Fatal("connect to provisioned tenant database:", err)
	}
	var admins int
	if err := tenantDB.QueryRowContext(ctx, db.Rebind(db.Resolve(driver), `SELECT COUNT(*) FROM _kora_user WHERE email = ?`), command.BootstrapAdminEmail).Scan(&admins); err != nil {
		tenantDB.Close()
		t.Fatal("read bootstrap administrator:", err)
	}
	if err := tenantDB.Close(); err != nil {
		t.Fatal("close tenant database:", err)
	}
	if admins != 1 {
		t.Fatalf("bootstrap administrator count = %d, want 1", admins)
	}
	firstRevision := info.ConfigRevision
	second, err := executor.Execute(ctx, managementTestClaim(operation))
	if err != nil || second.State != contract.OperationSucceeded {
		t.Fatalf("replayed provisioning execution = %+v, %v", second, err)
	}
	info, err = registry.GetByIDContext(ctx, string(command.Tenant))
	if err != nil || info.ConfigRevision != firstRevision {
		t.Fatalf("replay directory revision = %d, %v; want stable revision %d", info.ConfigRevision, err, firstRevision)
	}
	if coordinator.activated.Load() != 2 {
		t.Fatalf("runtime activation calls = %d, want 2", coordinator.activated.Load())
	}
}

func liveDatabaseTarget(driver, dsn string) (string, int, string, string, error) {
	if driver == "mysql" {
		parsed, err := mysql.ParseDSN(dsn)
		if err != nil {
			return "", 0, "", "", err
		}
		host, portText, found := strings.Cut(parsed.Addr, ":")
		if !found {
			return "", 0, "", "", fmt.Errorf("MySQL disposable DSN needs host:port")
		}
		port, err := strconv.Atoi(strings.TrimPrefix(portText, "//"))
		if err != nil {
			return "", 0, "", "", fmt.Errorf("MySQL disposable DSN port is invalid")
		}
		return host, port, parsed.User, parsed.Passwd, nil
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", 0, "", "", err
	}
	port := 5432
	if value := parsed.Port(); value != "" {
		port, err = strconv.Atoi(value)
		if err != nil {
			return "", 0, "", "", fmt.Errorf("PostgreSQL disposable DSN port is invalid")
		}
	}
	password, _ := parsed.User.Password()
	return parsed.Hostname(), port, parsed.User.Username(), password, nil
}
