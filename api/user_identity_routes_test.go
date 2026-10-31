package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/gin-gonic/gin"
)

func TestEngineDoesNotCreateDeleteOrResetCloudUserAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	api := router.Group("/api/v1")
	RegisterSystemRoutes(api, NewHandler(doctype.NewRegistry(), nil))

	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/system/users"},
		{http.MethodDelete, "/api/v1/system/users/cloud-user"},
		{http.MethodPost, "/api/v1/system/users/cloud-user/reset-password"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s %s returned %d, want 404", test.method, test.path, recorder.Code)
		}
	}
}
