package engine

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/site"
)

// A tenant installed from the pos pack stores its register view as
// {resources, components}. The page-manifest endpoints Studio waits on before
// it opens any record list must answer 200 and carry the view's resources.
func TestPageManifestEndpointsServePackRegisterView(t *testing.T) {
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	assertPageManifestEndpointsServeRegisterView(t, database, "libsql")
}

// assertPageManifestEndpointsServeRegisterView installs the pos pack on an
// empty tenant database and reads both page-manifest endpoints.
func assertPageManifestEndpointsServeRegisterView(t *testing.T, database *sql.DB, dbType string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dialect := db.Resolve(dbType)
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal(err)
	}
	const siteName = "pos-manifest.example.test"
	incoming, scripts, err := site.ParseConfigPack("retail-pos", composedRetailFiles(t))
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	store := configstore.NewStore(database, dialect)
	current, err := store.CollectSnapshot(doctype.NewRegistry(), siteName)
	if err != nil {
		t.Fatal(err)
	}
	target, err := mergeManagementPackageSnapshot(current, incoming)
	if err != nil {
		t.Fatalf("merge package: %v", err)
	}
	if err := site.ImportConfigFromSnapshotContext(t.Context(), database, doctype.NewRegistry(), "main", siteName, siteName, "install", target, dialect, scripts); err != nil {
		t.Fatalf("install package: %v", err)
	}
	var stored string
	if err := database.QueryRow(`SELECT config_json FROM _kora_view WHERE type = 'register'`).Scan(&stored); err != nil || stored == "" || stored[0] != '{' {
		t.Fatalf("register view should be stored in the object form, got %q (err %v)", stored, err)
	}

	handler := api.NewHandler(doctype.NewRegistry(), nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", siteName)
		c.Set("site_db", database)
		c.Set("site_db_type", dbType)
	})
	router.GET("/system/page-manifests", handler.HandleSystemPageManifests)
	router.GET("/page-manifests", handler.HandlePageManifestByRoute)
	get := func(path string) []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}

	var byRoute struct {
		Data doctype.PageManifest
	}
	var route string
	for _, v := range target.Views {
		if v.Type == "register" {
			route = v.Route
		}
	}
	if route == "" {
		t.Fatal("pack has no register view")
	}
	var list struct {
		Data []struct{ Name, Route string }
	}
	if err := json.Unmarshal(get("/system/page-manifests"), &list); err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, entry := range list.Data {
		listed = listed || entry.Route == route
	}
	if !listed {
		t.Fatalf("register route %s missing from the manifest list: %+v", route, list.Data)
	}
	if err := json.Unmarshal(get("/page-manifests?route="+route), &byRoute); err != nil {
		t.Fatal(err)
	}
	published := map[string]bool{}
	for _, resource := range byRoute.Data.Spec.Resources {
		if resource.Query == "published.query" {
			published[resource.ID] = true
		}
	}
	if !published["registers"] || len(byRoute.Data.Spec.Resources) != 5 {
		t.Fatalf("register manifest lacks its published.query resources: %+v", byRoute.Data.Spec.Resources)
	}
	var tender *doctype.PageComponent
	for i, child := range byRoute.Data.Spec.Layout.Children {
		if child.Component == "tender_panel" {
			tender = &byRoute.Data.Spec.Layout.Children[i]
		}
	}
	if tender == nil || tender.Version != 2 {
		t.Fatalf("tender_panel missing or lost its version: %+v", byRoute.Data.Spec.Layout.Children)
	}
	if ctx, _ := tender.Props["context"].(map[string]any); ctx["till_session"] != "session.name" {
		t.Fatalf("tender_panel props.context = %#v", tender.Props["context"])
	}
}
