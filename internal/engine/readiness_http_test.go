package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/gin-gonic/gin"
)

func TestPlatformReadinessHandlerRequiresDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/readyz", PlatformReadinessHandler(nil))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "{\"component\":\"platform_directory\",\"status\":\"not_ready\"}" {
		t.Fatalf("nil platform database readiness = %d %s", response.Code, response.Body.String())
	}

	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectPing()
	router = gin.New()
	router.GET("/readyz", PlatformReadinessHandler(database))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK || response.Body.String() != "{\"status\":\"ready\"}" {
		t.Fatalf("healthy platform database readiness = %d %s", response.Code, response.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformReadinessUsesAdmissionBeforePing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	admission, err := dbaccess.NewAdmission(1, 0, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	release, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing()
	router := gin.New()
	router.GET("/readyz", PlatformReadinessHandler(database, admission))
	rejected := httptest.NewRecorder()
	router.ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("capacity-rejected readiness = %d %s, want 503", rejected.Code, rejected.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("readiness ping ran without admission")
	}
	release()
	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("admitted readiness = %d %s, want 200", ready.Code, ready.Body.String())
	}
}

func TestPlatformHealthUsesEndpointAdmissionAndContextPing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	admission, err := dbaccess.NewAdmission(1, 0, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	release, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing()
	router := gin.New()
	if err := RegisterPlatformRoutes(PlatformRouteConfig{Router: router, PlatformDB: database, PlatformWorkAdmission: admission}); err != nil {
		t.Fatal(err)
	}
	degraded := httptest.NewRecorder()
	router.ServeHTTP(degraded, httptest.NewRequest(http.MethodGet, "/health", nil))
	if degraded.Code != http.StatusOK || degraded.Body.String() != "{\"db\":\"disconnected\",\"status\":\"degraded\"}" {
		t.Fatalf("capacity-rejected health = %d %s, want stable degraded response", degraded.Code, degraded.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("health ping ran without admission")
	}
	release()
	mock.ExpectPing()
	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/health", nil))
	if ready.Code != http.StatusOK || ready.Body.String() != "{\"db\":\"connected\",\"status\":\"ok\"}" {
		t.Fatalf("admitted health = %d %s, want connected response", ready.Code, ready.Body.String())
	}
}
