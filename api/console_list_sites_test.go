package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/doctype"
	knet "github.com/asenawritescode/kora/net"
	"github.com/gin-gonic/gin"
)

func TestConsoleSiteListPingsManagedDBThroughRuntimeHandle(t *testing.T) {
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing()
	mock.ExpectClose()
	runtime := &knet.LoadedSite{
		SiteID: "tenant-list-sites", Name: "list.example", Status: "active", ConfigRevision: 3,
		Config: knet.SiteRouterConfig{Hostname: "list.example", Domains: []string{"list.example"}},
		DB:     database, Registry: doctype.NewRegistry(),
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	handler := &ConsoleHandler{SiteRouter: router}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/console/sites", nil)
	handler.HandleListSites(ctx)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"active"`) {
		t.Fatalf("site list response=%d %s", recorder.Code, recorder.Body.String())
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
