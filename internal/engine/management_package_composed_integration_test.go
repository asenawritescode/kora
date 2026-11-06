//go:build integration

package engine

import (
	"context"
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

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/management"
	"github.com/asenawritescode/kora/management/sqlstore"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
)

// A template composing the inventory and pos packs (files under
// components/<ns>/, as Cloud's ResolveComposition produces) installs on a
// fresh tenant through the management package executor with the tenant's real
// runtime services, and a rebuilt runtime binds the generated modules.
func TestComposedTemplatePackageInstallsThroughExecutor(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	database, info, cleanup := openManagementPackageTestDatabase(t, "mysql", dsn)
	defer cleanup()
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	if err := site.BootstrapSystemTablesContext(ctx, database, dialect); err != nil {
		t.Fatal("bootstrap tenant database:", err)
	}
	loaded := installTenantServicesForTest(t, info, database)
	initialRevision, err := configstore.CurrentConfigRevision(ctx, database, dialect)
	if err != nil {
		t.Fatal(err)
	}

	manifest := contract.MarketplacePackageManifest{
		SchemaVersion: 1, PackageID: "retail-pos", ReleaseID: "retail-pos-1.0.1", Version: "1.0.1", Kind: "template",
		Public:      contract.ManifestPublic{Slug: "retail-pos", Name: "Retail POS", Summary: "Retail POS"},
		Engine:      contract.ManifestEngine{MinVersion: "1.0.0"},
		Composition: []contract.ManifestComponent{{ReleaseID: "inventory-1.0.1", Selection: "required"}, {ReleaseID: "pos-1.0.1", Selection: "required"}},
	}
	files := composedRetailFiles(t)
	canonical, err := contract.CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := contract.PackDigest(canonical, files)
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := contract.ParseTenantID(info.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	operationID := contract.OperationID(contract.NewID())
	command := contract.ApplyPackage{
		Meta:   contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("composed-" + operationID[len(operationID)-8:])},
		Tenant: tenant, ArtifactRef: manifest.ReleaseID, Digest: digest,
		ExpectedConfigRevision: contract.Revision(fmt.Sprint(initialRevision)),
	}
	artifactServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/engine/marketplace/releases/"+command.ArtifactRef || r.Header.Get("Authorization") != "Bearer composed-test-token" {
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
	resolver, err := NewHTTPManagementPackageArtifactResolver(artifactServer.URL, "composed-test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := newManagementPackageExecutor(packageExecutorDirectory{info: info}, packageExecutorAdmission{}, &packageExecutorCoordinator{}, knet.NewSiteRouter([]*knet.LoadedSite{loaded}), resolver)
	if err != nil {
		t.Fatal(err)
	}
	operationStore, err := sqlstore.New(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := operationStore.EnsureSchema(ctx); err != nil {
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
	if _, err := service.Accept(ctx, "cloud-service", command); err != nil {
		t.Fatal("accept composed package:", err)
	}
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(workerCtx, management.WorkerConfig{Owner: "composed-worker", Concurrency: 1, Lease: 10 * time.Second, Poll: 10 * time.Millisecond})
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
			t.Fatalf("composed package did not finish; last state %s", operation.State)
		case <-time.After(20 * time.Millisecond):
		}
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if operation.State != contract.OperationSucceeded {
		t.Fatalf("composed package operation = %s: %+v", operation.State, operation.Error)
	}

	modules, err := configstore.NewModuleStore(database, dialect).Load(tenant)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	operations := 0
	for _, module := range modules.Modules {
		ids = append(ids, string(module.ID))
		operations += len(module.PublishedOperations)
	}
	if strings.Join(ids, ",") != "inventory.app,inventory.core,pos.app,pos.core" || operations != 10 {
		t.Fatalf("installed modules = %v with %d published operations; want the four pack modules and 10", ids, operations)
	}
	rebuilt := installTenantServicesForTest(t, info, database)
	if plan := rebuilt.RuntimeServices.ModuleBindings; plan == nil || len(plan.Blockers) != 0 {
		t.Fatalf("rebuilt runtime module bindings = %+v", plan)
	}
}
