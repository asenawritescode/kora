package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHandleExtensionCreateRejectsInternalEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	endpoints := []string{
		"http://127.0.0.1:8000/api/system/permissions",
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:3306/",
		"http://10.0.0.5/hook",
		"https://db.internal/hook",
		"ftp://hooks.example.com/",
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint, func(t *testing.T) {
			h := NewHandler(nil, nil)
			r := gin.New()
			r.POST("/system/extension", h.HandleExtensionCreate)
			body := `{"name":"probe","endpoint_url":"` + endpoint + `"}`
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/system/extension", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "validation.invalid_endpoint_url") {
				t.Fatalf("status = %d body = %s, want 400 validation.invalid_endpoint_url", w.Code, w.Body.String())
			}
		})
	}
}
