package site

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	knet "github.com/asenawritescode/kora/net"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

// This integration test proves a committed SQL revision is consumed into the
// actual in-memory routing index, rather than testing the registry and router
// independently. It requires a disposable PostgreSQL registry database.
func TestLivePostgresDirectoryRevisionReplacesRoutedRuntime(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapPlatformRegistry(database, db.Resolve("postgres")); err != nil {
		t.Fatal("bootstrap platform registry:", err)
	}

	name := fmt.Sprintf("runtime-revision-%d.example.invalid", time.Now().UnixNano())
	registry := NewSQLSiteRegistry(database, "postgres")
	if err := registry.Upsert(&SiteConfig{Hostname: name, DomainsList: []string{name, "alias." + name}, DBType: "postgres", DBHost: "127.0.0.1", DBPort: 5432, DBName: "unused"}); err != nil {
		t.Fatal("create directory entry:", err)
	}
	defer func() {
		if err := removePlatformSiteRegistration(database, "postgres", name); err != nil {
			t.Errorf("remove integration site registration: %v", err)
		}
	}()
	initialInfo, err := registry.ResolveAlias(name)
	if err != nil {
		t.Fatal("resolve initial site:", err)
	}
	startCursor, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read initial directory cursor:", err)
	}
	initialRuntime := &knet.LoadedSite{
		SiteID: initialInfo.SiteID, ConfigRevision: initialInfo.ConfigRevision, Status: initialInfo.Status,
		Name: initialInfo.Name, Config: knet.SiteRouterConfig{Hostname: initialInfo.Name, Domains: initialInfo.Domains},
		RuntimeServices: initialInfo.ConfigRevision,
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{initialRuntime})
	consumer := &DirectoryConsumer{Registry: registry, ID: "integration:runtime-revision:" + name, InitialCursor: startCursor}
	consumer.Apply = func(_ context.Context, change SiteDirectoryChange) error {
		current := router.SiteByID(change.Descriptor.SiteID)
		if current == nil || current.ConfigRevision >= change.Descriptor.ConfigRevision {
			return nil
		}
		info, err := registry.GetByID(change.Descriptor.SiteID)
		if err != nil {
			return err
		}
		next := &knet.LoadedSite{
			SiteID: info.SiteID, ConfigRevision: info.ConfigRevision, Status: info.Status,
			Name: info.Name, Config: knet.SiteRouterConfig{Hostname: info.Name, Domains: info.Domains},
			RuntimeServices: info.ConfigRevision,
		}
		_, err = router.ReplaceSite(next)
		return err
	}
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("initialize consumer at snapshot boundary = %d, %v", count, err)
	}
	if err := registry.SetStatus(initialInfo.SiteID, "active"); err != nil {
		t.Fatal("commit newer config revision:", err)
	}
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("apply newer directory revision = %d, %v", count, err)
	}
	updated := router.SiteByID(initialInfo.SiteID)
	if updated == nil || updated == initialRuntime || updated.ConfigRevision != initialInfo.ConfigRevision+1 {
		t.Fatalf("router did not publish the database revision: initial=%#v updated=%#v", initialRuntime, updated)
	}
	gin.SetMode(gin.TestMode)
	requestRouter := gin.New()
	requestRouter.Use(router.Middleware())
	requestRouter.GET("/runtime", func(c *gin.Context) {
		bundle, _ := c.Get("site_runtime_services")
		c.String(200, fmt.Sprint(bundle))
	})
	request := httptest.NewRequest("GET", "/runtime", nil)
	request.Host = "alias." + name
	response := httptest.NewRecorder()
	requestRouter.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != fmt.Sprint(updated.ConfigRevision) {
		t.Fatalf("alias request did not receive refreshed runtime bundle: status=%d body=%q want=%d", response.Code, response.Body.String(), updated.ConfigRevision)
	}
}
