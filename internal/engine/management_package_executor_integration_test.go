//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/management/sqlstore"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/tenantruntime"
	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

type packageExecutorDirectory struct{ info site.DBSiteInfo }

func (d packageExecutorDirectory) GetByIDContext(_ context.Context, id string) (site.DBSiteInfo, error) {
	if id != d.info.SiteID {
		return site.DBSiteInfo{}, sql.ErrNoRows
	}
	return d.info, nil
}
func (packageExecutorDirectory) CurrentDirectoryRevisionContext(context.Context) (uint64, error) {
	return 3, nil
}

type packageExecutorAdmission struct{}

func (packageExecutorAdmission) Acquire(context.Context) (func(), error) { return func() {}, nil }

type packageExecutorCoordinator struct {
	replaces        int
	replaceFailures int
}

type packageExecutorIDs struct{ id string }

func (s packageExecutorIDs) NewID() string { return s.id }

func (*packageExecutorCoordinator) ActivateFromDirectory(context.Context, string) error { return nil }
func (*packageExecutorCoordinator) Retire(context.Context, string) error                { return nil }
func (c *packageExecutorCoordinator) Replace(context.Context, site.DBSiteInfo, uint64) error {
	c.replaces++
	if c.replaceFailures > 0 {
		c.replaceFailures--
		return fmt.Errorf("injected transient runtime replacement failure")
	}
	return nil
}

func TestLiveManagementPackageExecutorAcrossDialects(t *testing.T) {
	tests := []struct {
		name, driver, env string
	}{
		{name: "mysql", driver: "mysql", env: "KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"},
		{name: "postgres", driver: "postgres", env: "KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN"},
		{name: "libsql_http", driver: "libsql", env: "KORA_MANAGEMENT_DISPOSABLE_LIBSQL_DSN"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable loopback SQL service", test.env)
			}
			database, info, cleanup := openManagementPackageTestDatabase(t, test.driver, dsn)
			defer cleanup()
			database.SetMaxOpenConns(4)
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			dialect := db.Resolve(test.driver)
			if _, err := database.Exec(`CREATE TABLE _kora_system_bootstrap (id INTEGER PRIMARY KEY, version INTEGER NOT NULL)`); err != nil {
				t.Fatal("create prior bootstrap marker:", err)
			}
			if _, err := database.Exec(`INSERT INTO _kora_system_bootstrap (id, version) VALUES (1, 21)`); err != nil {
				t.Fatal("seed prior bootstrap version:", err)
			}
			if err := site.BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatal("bootstrap isolated tenant database:", err)
			}
			initialRevision, err := configstore.CurrentConfigRevision(ctx, database, dialect)
			if err != nil {
				t.Fatal("read initial tenant config revision:", err)
			}
			if _, err := configstore.NewStore(database, dialect).CollectSnapshot(doctype.NewRegistry(), info.Name); err != nil {
				t.Fatal("read baseline tenant configuration snapshot:", err)
			}

			manifest := contract.MarketplacePackageManifest{
				SchemaVersion: 1, PackageID: "inventory", ReleaseID: "inventory-1", Version: "1.0.0", Kind: "module",
				Public: contract.ManifestPublic{Slug: "inventory", Name: "Inventory", Summary: "Inventory"},
				Engine: contract.ManifestEngine{MinVersion: "1.0.0"},
				Modules: []contract.ModuleDescriptor{{
					ID: "inventory.stock", Version: "1.0.0",
					Requires: []contract.CapabilityRequirement{{Name: "warehouse.lookup", Contract: contract.SchemaRef{ID: "warehouse.stock", Version: 1}, Required: true}},
					Resources: []contract.OwnedResource{{
						Ref:    contract.ResourceRef{Namespace: "tenant", Name: "Stock", Version: 1},
						Access: contract.ModulePrivate, MutationMode: contract.MutationCRUD,
						ScopeKind: contract.ScopeTenantShared,
					}},
				}},
			}
			files := []contract.PackFile{{Path: "doctypes/Stock.yaml", ContentType: "doctype", Content: "name: Stock\nmodule: Inventory\nfields:\n  - fieldname: title\n    fieldtype: Data\n    search_index: true\n"}}
			canonical, err := contract.CanonicalManifestJSON(manifest)
			if err != nil {
				t.Fatal("canonicalize package manifest:", err)
			}
			digest, err := contract.PackDigest(canonical, files)
			if err != nil {
				t.Fatal("digest package:", err)
			}
			tenant, err := contract.ParseTenantID(info.SiteID)
			if err != nil {
				t.Fatal("parse test tenant:", err)
			}
			operationID := contract.OperationID(contract.NewID())
			command := contract.ApplyPackage{
				Meta:   contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("package-live-" + operationID[len(operationID)-8:])},
				Tenant: tenant, ArtifactRef: manifest.ReleaseID, Digest: digest,
				ExpectedConfigRevision: contract.Revision(fmt.Sprint(initialRevision)),
			}
			providerSelection := contract.ProviderSelection{Key: contract.BindingKey{
				Scope:            contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant},
				ConsumerModuleID: "inventory.stock", CapabilityName: "warehouse.lookup",
			}, ProviderID: "external-warehouse"}
			command.ProviderSelections = []contract.ProviderSelection{providerSelection}
			artifactServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/internal/engine/marketplace/releases/"+command.ArtifactRef || r.URL.Query().Get("digest") != digest {
					http.NotFound(w, r)
					return
				}
				if r.Header.Get("Authorization") != "Bearer live-package-test-token" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Kora-Artifact-Digest", digest)
				_ = json.NewEncoder(w).Encode(struct {
					Manifest contract.MarketplacePackageManifest `json:"manifest"`
					Files    []contract.PackFile                 `json:"files"`
					Digest   string                              `json:"digest"`
				}{Manifest: manifest, Files: files, Digest: digest})
			}))
			defer artifactServer.Close()
			resolver, err := NewHTTPManagementPackageArtifactResolver(artifactServer.URL, "live-package-test-token", nil)
			if err != nil {
				t.Fatal("construct live Engine artifact resolver:", err)
			}
			// Simulate a process crash after the first non-transactional DDL
			// statement. The executor below starts with only durable DB state; it
			// must re-plan live schema and restore any remaining indexes/triggers.
			incoming, _, _, err := verifiedManagementPackage(ctx, resolver, command)
			if err != nil {
				t.Fatal("verify package before simulating interruption:", err)
			}
			packageStore := configstore.NewStore(database, dialect)
			currentSnapshot, err := packageStore.CollectSnapshot(doctype.NewRegistry(), info.Name)
			if err != nil {
				t.Fatal("collect current config before interruption:", err)
			}
			targetSnapshot, err := mergeManagementPackageSnapshot(currentSnapshot, incoming)
			if err != nil {
				t.Fatal("merge package before interruption:", err)
			}
			if _, err := configstore.EnsureActivationOperation(database, dialect, info.Name, string(operationID), "management_package", targetSnapshot); err != nil {
				t.Fatal("persist interrupted package activation:", err)
			}
			plannedDDL, err := schema.PlanSnapshotActivation(database, info.DBName, dialect, doctype.NewRegistry(), targetSnapshot, "", string(operationID))
			if err != nil || len(plannedDDL) < 2 {
				t.Fatalf("initial package DDL plan = %v, %v; want table plus index/trigger", plannedDDL, err)
			}
			planJSON, err := json.Marshal(plannedDDL)
			if err != nil {
				t.Fatal("encode interrupted DDL plan:", err)
			}
			if err := configstore.UpdateActivationOperation(database, dialect, info.Name, string(operationID), "management_package", configstore.ActivationApplying, string(planJSON), "replan-from-live-schema", "simulated process interruption"); err != nil {
				t.Fatal("mark package DDL as applying:", err)
			}
			if err := schema.ApplySnapshotActivationDDL(ctx, database, dialect, plannedDDL[:1]); err != nil {
				t.Fatal("apply DDL prefix before simulated crash:", err)
			}
			providerAvailable := true
			candidatePreflights := 0
			runtime := &knet.LoadedSite{
				SiteID: info.SiteID, Name: info.Name, Status: "active", DBType: test.driver,
				DB: database, Registry: doctype.NewRegistry(), ConfigRevision: initialRevision,
				Config: knet.SiteRouterConfig{Hostname: info.Name, Domains: []string{info.Name}},
				RuntimeServices: &tenantruntime.Services{ResolveCandidateModuleBindings: func(scope contract.BusinessScope, packages []contract.InstalledPackage, modules []contract.ModuleDescriptor, _ *doctype.ConfigSnapshot, selections []contract.ProviderSelection, revision uint64) (plugin.BindingPlan, error) {
					candidatePreflights++
					if !providerAvailable {
						return plugin.BindingPlan{}, errors.New("external warehouse is offline")
					}
					pluginSelections := make([]plugin.ProviderSelection, 0, len(selections))
					for _, selection := range selections {
						pluginSelections = append(pluginSelections, plugin.ProviderSelection{Key: plugin.BindingKey{Scope: selection.Key.Scope, ConsumerModuleID: selection.Key.ConsumerModuleID, CapabilityName: selection.Key.CapabilityName}, ProviderID: selection.ProviderID})
					}
					return plugin.ResolveInstalledModules(scope, plugin.InstalledModules{Packages: packages, Modules: modules, Providers: []plugin.ProviderRegistration{{
						TenantID: tenant, ProviderID: "external-warehouse", OwnerModuleID: "warehouse.external", Kind: plugin.ProviderExternal,
						Offers:    []contract.CapabilityOffer{{Name: "warehouse.lookup", Contract: contract.SchemaRef{ID: "warehouse.stock", Version: 1}}},
						Factories: map[contract.CapabilityName]plugin.ProviderFactory{"warehouse.lookup": plugin.NewStaticFactory("warehouse.lookup", nil)},
					}}, Selections: pluginSelections, Revision: revision})
				}},
			}
			router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
			coordinator := &packageExecutorCoordinator{replaceFailures: 1}
			executor, err := newManagementPackageExecutor(packageExecutorDirectory{info: info}, packageExecutorAdmission{}, coordinator, router, resolver)
			if err != nil {
				t.Fatal("construct package executor:", err)
			}
			operationStore, err := sqlstore.New(database, dialect)
			if err != nil {
				t.Fatal("construct durable Engine operation store:", err)
			}
			if err := operationStore.EnsureSchema(ctx); err != nil {
				t.Fatal("create durable Engine operation schema:", err)
			}
			service, err := management.NewService(operationStore, packageExecutorIDs{id: string(operationID)}, time.Now)
			if err != nil {
				t.Fatal("construct management operation service:", err)
			}
			worker, err := management.NewWorker(operationStore, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal("construct durable management worker:", err)
			}
			startWorker := func(worker *management.Worker) (context.CancelFunc, <-chan error) {
				workerCtx, stop := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() {
					done <- worker.Run(workerCtx, management.WorkerConfig{Owner: "package-worker", Concurrency: 1, Lease: 3 * time.Second, Poll: 10 * time.Millisecond})
				}()
				return stop, done
			}
			waitForState := func(operationService *management.Service, id contract.OperationID, want contract.OperationState) management.Operation {
				t.Helper()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					operation, getErr := operationService.Get(ctx, "cloud-service", id)
					if getErr != nil {
						t.Fatalf("read package operation while waiting for %s: %v", want, getErr)
					}
					if operation.State == want {
						return operation
					}
					select {
					case <-ctx.Done():
						t.Fatalf("package operation did not reach %s; last state %s: %v", want, operation.State, ctx.Err())
					case <-ticker.C:
					}
				}
			}
			providerAvailable = false
			failedCommand := command
			failedCommand.Meta.IdempotencyKey = contract.IdempotencyKey("package-preflight-" + operationID[len(operationID)-8:])
			preflightOperationID := contract.OperationID(contract.NewID())
			preflightService, err := management.NewService(operationStore, packageExecutorIDs{id: string(preflightOperationID)}, time.Now)
			if err != nil {
				t.Fatal("construct failed-preflight operation service:", err)
			}
			preflightFailed, err := preflightService.Accept(ctx, "cloud-service", failedCommand)
			if err != nil || preflightFailed.State != contract.OperationRequested {
				t.Fatalf("accept package with unavailable mandatory provider = %+v, %v", preflightFailed, err)
			}
			stopWorker, workerDone := startWorker(worker)
			failedOperation := waitForState(preflightService, preflightFailed.ID, contract.OperationFailed)
			stopWorker()
			if err := <-workerDone; err != nil {
				t.Fatal("join failed-preflight management worker:", err)
			}
			if failedOperation.Error == nil || candidatePreflights != 1 || coordinator.replaces != 0 {
				t.Fatalf("failed composition preflight did not stop before runtime replacement: operation=%+v preflights=%d replacements=%d", failedOperation, candidatePreflights, coordinator.replaces)
			}
			if revision, err := configstore.CurrentConfigRevision(ctx, database, dialect); err != nil || revision != initialRevision {
				t.Fatalf("failed composition preflight changed config revision to %d: %v", revision, err)
			}
			preflightSnapshot, err := configstore.NewModuleStore(database, dialect).Load(tenant)
			if err != nil || preflightSnapshot.Revision != 0 || len(preflightSnapshot.Packages) != 0 || len(preflightSnapshot.Modules) != 0 {
				t.Fatalf("failed composition preflight changed installed modules: %+v, %v", preflightSnapshot, err)
			}
			providerAvailable = true
			accepted, err := service.Accept(ctx, "cloud-service", command)
			if err != nil || accepted.ID != operationID || accepted.State != contract.OperationRequested {
				t.Fatalf("accept package operation = %+v, %v", accepted, err)
			}
			stopWorker, workerDone = startWorker(worker)
			uncertain := waitForState(service, operationID, contract.OperationRecoveryRequired)
			stopWorker()
			if err := <-workerDone; err != nil {
				t.Fatal("join first management worker:", err)
			}
			if uncertain.Error == nil {
				t.Fatal("package runtime replacement failure was not reported as recoverable")
			}
			providerAvailable = false
			if revision, err := configstore.CurrentConfigRevision(ctx, database, dialect); err != nil || revision != initialRevision+1 {
				t.Fatalf("committed tenant config revision after runtime outage = %d, %v; want %d", revision, err, initialRevision+1)
			}
			recovery := contract.ReconcileTenantPackageOperation{
				Meta:   contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("package-recovery-" + operationID[len(operationID)-8:])},
				Reason: "test verified the interrupted activation journal and tenant state",
			}
			reconciled, err := service.ReconcileTenantPackage(ctx, "cloud-service", operationID, recovery)
			if err != nil || reconciled.ID != operationID || reconciled.State != contract.OperationRequested || reconciled.Error != nil {
				t.Fatalf("reconcile package operation through service = %+v, %v", reconciled, err)
			}
			// Reopen the durable store before resuming to exercise process restart
			// recovery rather than reusing the first worker's in-memory objects.
			operationStore, err = sqlstore.New(database, dialect)
			if err != nil {
				t.Fatal("reopen durable Engine operation store:", err)
			}
			service, err = management.NewService(operationStore, packageExecutorIDs{id: string(operationID)}, time.Now)
			if err != nil {
				t.Fatal("reopen management operation service:", err)
			}
			worker, err = management.NewWorker(operationStore, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal("reopen durable management worker:", err)
			}
			stopWorker, workerDone = startWorker(worker)
			succeeded := waitForState(service, operationID, contract.OperationSucceeded)
			stopWorker()
			if err := <-workerDone; err != nil {
				t.Fatal("join recovered management worker:", err)
			}
			if succeeded.ObservedRevision != contract.Revision(fmt.Sprint(initialRevision+1)) || succeeded.Error != nil {
				t.Fatalf("recovered package operation = %+v; expected successful revision %d", succeeded, initialRevision+1)
			}
			if coordinator.replaces != 2 {
				t.Fatalf("runtime replacements = %d, want failed attempt plus recovered attempt", coordinator.replaces)
			}
			if candidatePreflights != 2 {
				t.Fatalf("candidate binding preflights = %d, want failed preflight plus one before commit; committed recovery must not depend on current provider health", candidatePreflights)
			}
			if revision, err := configstore.CurrentConfigRevision(ctx, database, dialect); err != nil || revision != initialRevision+1 {
				t.Fatalf("tenant config revision after recovery = %d, %v; want one committed revision %d", revision, err, initialRevision+1)
			}
			var activeVersions int
			if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND status = 'Active'`), info.Name).Scan(&activeVersions); err != nil || activeVersions != 1 {
				t.Fatalf("active config version count = %d, %v; want 1", activeVersions, err)
			}
			journal, err := configstore.LoadActivationOperation(database, dialect, info.Name, string(operationID), "management_package")
			if err != nil || journal.Phase != configstore.ActivationCompleted {
				t.Fatalf("package activation journal = %+v, %v; want completed", journal, err)
			}
			var doctypeCount int
			if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_doctype WHERE site = ? AND name = ?`), info.Name, "Stock").Scan(&doctypeCount); err != nil || doctypeCount != 1 {
				t.Fatalf("activated package doctype count = %d, %v; want 1", doctypeCount, err)
			}
			if test.driver == "libsql" {
				var triggerCount int
				if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, "tabStock_modified_on_update").Scan(&triggerCount); err != nil || triggerCount != 1 {
					t.Fatalf("recovered LibSQL modified trigger count = %d, %v; want 1", triggerCount, err)
				}
			}
			modules, err := configstore.NewModuleStore(database, dialect).Load(contract.TenantID(info.SiteID))
			if err != nil {
				t.Fatal("reload durable module ownership snapshot after activation recovery:", err)
			}
			if modules.Revision != 1 || len(modules.Modules) != 1 || modules.Modules[0].ID != "inventory.stock" || len(modules.Installations) != 1 || modules.Installations[0].ArtifactDigest != digest || len(modules.Ownership) != 1 || modules.Ownership[0].OwnerModuleID != "inventory.stock" || modules.Ownership[0].Resource.Name != "Stock" {
				t.Fatalf("recovered module snapshot = %+v; want active inventory module, digest, and Stock ownership", modules)
			}
			moduleStore := configstore.NewModuleStore(database, dialect)
			if len(modules.ProviderSelections) != 1 || modules.ProviderSelections[0].ProviderID != "external-warehouse" {
				t.Fatalf("package activation did not persist selected external provider: %+v", modules.ProviderSelections)
			}
			pinned := modules
			start := make(chan struct{})
			results := make(chan error, 2)
			var writers sync.WaitGroup
			for _, candidateDigest := range []string{"concurrent-digest-a", "concurrent-digest-b"} {
				candidate := pinned
				candidate.Installations = append([]contract.ModuleInstallation(nil), pinned.Installations...)
				candidate.Installations[0].ArtifactDigest = candidateDigest
				writers.Add(1)
				go func(candidate contract.ModuleSnapshot) {
					defer writers.Done()
					tx, beginErr := database.BeginTx(ctx, nil)
					if beginErr != nil {
						results <- beginErr
						return
					}
					<-start
					if _, commitErr := moduleStore.CommitActivation(ctx, tx, pinned.Revision, candidate); commitErr != nil {
						_ = tx.Rollback()
						results <- commitErr
						return
					}
					results <- tx.Commit()
				}(candidate)
			}
			close(start)
			writers.Wait()
			close(results)
			wins := 0
			for result := range results {
				if result == nil {
					wins++
				}
			}
			if wins != 1 {
				t.Fatalf("concurrent module snapshot writers committed %d times; want one", wins)
			}
			concurrent, err := moduleStore.Load(contract.TenantID(info.SiteID))
			if err != nil || concurrent.Revision != pinned.Revision+1 || len(concurrent.ProviderSelections) != 1 || concurrent.ProviderSelections[0].ProviderID != "external-warehouse" || (concurrent.Installations[0].ArtifactDigest != "concurrent-digest-a" && concurrent.Installations[0].ArtifactDigest != "concurrent-digest-b") {
				t.Fatalf("module snapshot after concurrent activation = %+v, %v; want revision %d, pinned provider, and one winning digest", concurrent, err, pinned.Revision+1)
			}
		})
	}
}

func openManagementPackageTestDatabase(t *testing.T, driver, dsn string) (*sql.DB, site.DBSiteInfo, func()) {
	t.Helper()
	name := fmt.Sprintf("kora_pkg_%d", time.Now().UnixNano()%1_000_000_000)
	switch driver {
	case "mysql":
		configuration, err := mysql.ParseDSN(dsn)
		if err != nil || (!strings.Contains(configuration.Addr, "127.0.0.1:") && !strings.Contains(configuration.Addr, "localhost:")) {
			t.Fatal("refusing non-loopback MySQL test DSN")
		}
		configuration.DBName = ""
		root, err := sql.Open(driver, configuration.FormatDSN())
		if err != nil {
			t.Fatal("open disposable MySQL service:", err)
		}
		if _, err := root.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4"); err != nil {
			root.Close()
			t.Fatal("create isolated MySQL database:", err)
		}
		configuration.DBName = name
		database, err := sql.Open(driver, configuration.FormatDSN())
		if err != nil {
			_, _ = root.Exec("DROP DATABASE " + name)
			root.Close()
			t.Fatal("open isolated MySQL database:", err)
		}
		host, portText, found := strings.Cut(configuration.Addr, ":")
		if !found {
			_ = database.Close()
			_, _ = root.Exec("DROP DATABASE " + name)
			_ = root.Close()
			t.Fatal("MySQL integration DSN must include a host and port")
		}
		port := 0
		if _, err := fmt.Sscan(portText, &port); err != nil || port < 1 {
			_ = database.Close()
			_, _ = root.Exec("DROP DATABASE " + name)
			_ = root.Close()
			t.Fatal("MySQL integration DSN has an invalid port")
		}
		info := site.DBSiteInfo{SiteID: "tenant-package-live", Name: "tenant-package-live.example.test", Status: "active", DBType: driver, DBHost: host, DBPort: port, DBName: name, DBUser: configuration.User, DBPassword: configuration.Passwd, FileStorage: "local"}
		return database, info, func() { _ = database.Close(); _, _ = root.Exec("DROP DATABASE IF EXISTS " + name); _ = root.Close() }
	case "postgres":
		parsed, err := url.Parse(dsn)
		if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
			t.Fatal("refusing non-loopback PostgreSQL test DSN")
		}
		root, err := sql.Open(driver, dsn)
		if err != nil {
			t.Fatal("open disposable PostgreSQL service:", err)
		}
		if _, err := root.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
			root.Close()
			t.Fatal("create isolated PostgreSQL database:", err)
		}
		isolated := *parsed
		isolated.Path = "/" + name
		database, err := sql.Open(driver, isolated.String())
		if err != nil {
			_, _ = root.Exec(`DROP DATABASE "` + name + `"`)
			root.Close()
			t.Fatal("open isolated PostgreSQL database:", err)
		}
		port := 5432
		if parsed.Port() != "" {
			_, _ = fmt.Sscan(parsed.Port(), &port)
		}
		password, _ := parsed.User.Password()
		info := site.DBSiteInfo{SiteID: "tenant-package-live", Name: "tenant-package-live.example.test", Status: "active", DBType: driver, DBHost: parsed.Hostname(), DBPort: port, DBName: name, DBUser: parsed.User.Username(), DBPassword: password, FileStorage: "local"}
		return database, info, func() { _ = database.Close(); _, _ = root.Exec(`DROP DATABASE "` + name + `"`); _ = root.Close() }
	default:
		parsed, err := url.Parse(dsn)
		if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
			t.Fatal("refusing non-loopback LibSQL test DSN")
		}
		database, err := sql.Open(driver, dsn)
		if err != nil {
			t.Fatal("open disposable LibSQL HTTP service:", err)
		}
		var tables int
		if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil || tables != 0 {
			database.Close()
			t.Fatalf("refusing non-empty LibSQL test database with %d tables: %v", tables, err)
		}
		info := site.DBSiteInfo{SiteID: "tenant-package-live", Name: "tenant-package-live.example.test", Status: "active", DBType: driver, DBHost: dsn, DBName: name, FileStorage: "local"}
		return database, info, func() { _ = database.Close() }
	}
}
