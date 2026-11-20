//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/management/sqlstore"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
)

// kitchenOrderWorkflow is a package workflow definition that moves a Kitchen
// Order from New to the target state through a record.update_status step.
func kitchenOrderWorkflow(version uint32, to string) string {
	return fmt.Sprintf(`{"id":"kitchen-order-start","version":%d,"entry":"transition","trigger":{"kind":"record.updated","table":"Kitchen Order","watched_fields":["status"]},"steps":[{"id":"transition","kind":"action","action":{"name":"record.update_status","version":1,"input_schema":"kora.record.update_status","input":{"state_field":"status","initial_state":"New","transition_action":"Start cooking","from":"New","to":%q,"allowed_roles":["Administrator","Restaurant Manager","Waiter"]},"next":"finish"}},{"id":"finish","kind":"end","end":{"outcome":"complete"}}]}`, version, to)
}

// kitchenPackage builds a managed package holding the Kitchen Order doctype and
// one workflow definition.
func kitchenPackage(t *testing.T, version string, workflow string) (contract.MarketplacePackageManifest, []contract.PackFile, string) {
	t.Helper()
	manifest := contract.MarketplacePackageManifest{
		SchemaVersion: 1, PackageID: "kitchen", ReleaseID: "kitchen-" + version, Version: version, Kind: "module",
		Public: contract.ManifestPublic{Slug: "kitchen", Name: "Kitchen", Summary: "Kitchen"},
		Engine: contract.ManifestEngine{MinVersion: "1.0.0"},
		Modules: []contract.ModuleDescriptor{{
			// The module descriptor stays fixed: the merge refuses a changed module without an authorized replacement.
			ID: "kitchen.orders", Version: "1.0.0",
			Resources: []contract.OwnedResource{{
				Ref:    contract.ResourceRef{Namespace: "tenant", Name: "Kitchen Order", Version: 1},
				Access: contract.ModulePrivate, MutationMode: contract.MutationCRUD,
				ScopeKind: contract.ScopeTenantShared,
			}},
		}},
	}
	files := []contract.PackFile{
		{Path: "doctypes/KitchenOrder.yaml", ContentType: "doctype", Content: "name: Kitchen Order\nmodule: Kitchen\nfields:\n  - fieldname: status\n    fieldtype: Select\n    options: |\n      New\n      Preparing\n      Cooking\n"},
		{Path: "workflows/kitchen-order-start.json", ContentType: "workflow_definition", Content: workflow},
	}
	canonical, err := contract.CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatal("canonicalize kitchen manifest:", err)
	}
	digest, err := contract.PackDigest(canonical, files)
	if err != nil {
		t.Fatal("digest kitchen package:", err)
	}
	return manifest, files, digest
}

// applyManagedPackage runs one package application through the real executor and
// worker, as Cloud's management operation does, and returns the terminal operation.
func applyManagedPackage(t *testing.T, ctx context.Context, database *sql.DB, info site.DBSiteInfo, dialect db.Dialect, manifest contract.MarketplacePackageManifest, files []contract.PackFile, digest string) management.Operation {
	t.Helper()
	tenant, err := contract.ParseTenantID(info.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := configstore.CurrentConfigRevision(ctx, database, dialect)
	if err != nil {
		t.Fatal("read config revision:", err)
	}
	operationID := contract.OperationID(contract.NewID())
	command := contract.ApplyPackage{
		Meta:   contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("kitchen-" + string(operationID)[len(operationID)-12:])},
		Tenant: tenant, ArtifactRef: manifest.ReleaseID, Digest: digest,
		ExpectedConfigRevision: contract.Revision(fmt.Sprint(revision)),
	}
	artifactServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/engine/marketplace/releases/"+command.ArtifactRef || r.Header.Get("Authorization") != "Bearer kitchen-test-token" {
			http.NotFound(w, r)
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
	resolver, err := NewHTTPManagementPackageArtifactResolver(artifactServer.URL, "kitchen-test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the runtime for each application so it plans from the committed config.
	loaded := installTenantServicesForTest(t, info, database)
	executor, err := newManagementPackageExecutor(packageExecutorDirectory{info: info}, packageExecutorAdmission{}, &packageExecutorCoordinator{}, knet.NewSiteRouter([]*knet.LoadedSite{loaded}), resolver)
	if err != nil {
		t.Fatal(err)
	}
	operationStore, err := sqlstore.New(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err = operationStore.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := management.NewService(operationStore, packageExecutorIDs{id: string(operationID)}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := management.NewWorker(operationStore, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Accept(ctx, "cloud-service", command); err != nil {
		t.Fatal("accept kitchen package:", err)
	}
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(workerCtx, management.WorkerConfig{Owner: "kitchen-worker", Concurrency: 1, Lease: 10 * time.Second, Poll: 10 * time.Millisecond})
	}()
	var operation management.Operation
	for {
		operation, err = service.Get(ctx, "cloud-service", operationID)
		if err != nil {
			t.Fatal(err)
		}
		if operation.State == contract.OperationSucceeded || operation.State == contract.OperationFailed || operation.State == contract.OperationRecoveryRequired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("kitchen package did not finish; last state %s", operation.State)
		case <-time.After(20 * time.Millisecond):
		}
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return operation
}

func TestManagedPackageWorkflowsPublishThroughExecutor(t *testing.T) {
	tests := []struct{ name, driver, env string }{
		{name: "mysql", driver: "mysql", env: "KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"},
		{name: "postgres", driver: "postgres", env: "KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable loopback SQL service", test.env)
			}
			database, info, cleanup := openManagementPackageTestDatabase(t, test.driver, dsn)
			defer cleanup()
			ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
			defer cancel()
			dialect := db.Resolve(test.driver)
			if err := site.BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatal("bootstrap tenant database:", err)
			}
			tenant, err := contract.ParseTenantID(info.SiteID)
			if err != nil {
				t.Fatal(err)
			}
			definitions, err := workflowsql.NewDefinitionStore(database, dialect)
			if err != nil {
				t.Fatal(err)
			}
			statusOf := func(to string) {
				t.Helper()
				policy, ok, lookupErr := definitions.GetStatusPolicy(ctx, tenant, "Kitchen Order")
				if lookupErr != nil || !ok {
					t.Fatalf("status policy for Kitchen Order = %+v, %v, %v; want published transitions", policy, ok, lookupErr)
				}
				if policy.StateField != "status" || policy.InitialState != "New" || len(policy.Transitions) != 1 || policy.Transitions[0].From != "New" || policy.Transitions[0].To != to {
					t.Fatalf("status policy = %+v; want one New -> %s transition", policy, to)
				}
			}
			publishedRows := func() int {
				t.Helper()
				var count int
				query := db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_workflow_definition WHERE tenant_id = ?`)
				if err = database.QueryRowContext(ctx, query, string(tenant)).Scan(&count); err != nil {
					t.Fatal("count published workflow definitions:", err)
				}
				return count
			}

			manifest, files, digest := kitchenPackage(t, "1.0.0", kitchenOrderWorkflow(1, "Preparing"))
			if op := applyManagedPackage(t, ctx, database, info, dialect, manifest, files, digest); op.State != contract.OperationSucceeded {
				t.Fatalf("install v1 = %s: %+v", op.State, op.Error)
			}
			statusOf("Preparing")
			if n := publishedRows(); n != 1 {
				t.Fatalf("published rows after install = %d; want 1", n)
			}

			// Re-applying the same package must succeed without changing what is published.
			revisionBeforeReapply, err := configstore.CurrentConfigRevision(ctx, database, dialect)
			if err != nil {
				t.Fatal(err)
			}
			if op := applyManagedPackage(t, ctx, database, info, dialect, manifest, files, digest); op.State != contract.OperationSucceeded {
				t.Fatalf("re-apply = %s: %+v", op.State, op.Error)
			}
			statusOf("Preparing")
			if n := publishedRows(); n != 1 {
				t.Fatalf("published rows after re-apply = %d; want 1", n)
			}
			if revision, revErr := configstore.CurrentConfigRevision(ctx, database, dialect); revErr != nil || revision != revisionBeforeReapply+1 {
				t.Fatalf("re-apply config revision = %d, %v; want %d", revision, err, revisionBeforeReapply+1)
			}

			// A same-version definition with different content must refuse the whole
			// activation: nothing is published and the config revision stays put.
			conflictManifest, conflictFiles, conflictDigest := kitchenPackage(t, "1.0.1", kitchenOrderWorkflow(1, "Cooking"))
			revisionBeforeConflict, err := configstore.CurrentConfigRevision(ctx, database, dialect)
			if err != nil {
				t.Fatal(err)
			}
			if op := applyManagedPackage(t, ctx, database, info, dialect, conflictManifest, conflictFiles, conflictDigest); op.State != contract.OperationFailed || op.Error == nil || op.Error.Type != contract.CodeConflict {
				t.Fatalf("conflicting workflow version = %s: %+v; want failed conflict", op.State, op.Error)
			}
			if revision, revErr := configstore.CurrentConfigRevision(ctx, database, dialect); revErr != nil || revision != revisionBeforeConflict {
				t.Fatalf("refused activation changed config revision to %d: %v", revision, err)
			}
			statusOf("Preparing")

			// An upgrade with a new definition version publishes that version, and it
			// becomes the policy the table reads.
			upgradeManifest, upgradeFiles, upgradeDigest := kitchenPackage(t, "1.1.0", kitchenOrderWorkflow(2, "Cooking"))
			if op := applyManagedPackage(t, ctx, database, info, dialect, upgradeManifest, upgradeFiles, upgradeDigest); op.State != contract.OperationSucceeded {
				t.Fatalf("upgrade to workflow v2 = %s: %+v", op.State, op.Error)
			}
			statusOf("Cooking")
			if n := publishedRows(); n != 2 {
				t.Fatalf("published rows after upgrade = %d; want both versions", n)
			}
		})
	}
}
