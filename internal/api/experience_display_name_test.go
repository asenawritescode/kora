package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

// The provisioning display name is stored as branding.app_name; Studio reads
// it from the tenant experience for sign-in, sidebar and page titles.
func TestTenantExperienceAppNameUsesProvisionedDisplayName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const hostname = "dbg-duka-d6f2364e047c7286.kigae.app"
	for _, tt := range []struct {
		name   string
		stored *string
		want   string
	}{
		{"legacy tenant keeps hostname label", nil, "Dbg Duka D6f2364e047c7286 Workspace"},
		{"provisioned display name", strPtr("Dbg Duka"), "Dbg Duka"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows := sqlmock.NewRows([]string{"setting_key", "setting_value"})
			if tt.stored != nil {
				rows.AddRow("branding.app_name", *tt.stored)
			}
			mock.ExpectQuery("SELECT setting_key, setting_value FROM _kora_site_setting").WithArgs(hostname).WillReturnRows(rows)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/system/experience", nil)
			c.Set("site_db", db)
			c.Set("site_name", hostname)
			c.Set("site_registry", doctype.NewRegistry())
			NewHandler(doctype.NewRegistry(), nil).HandleSystemBranding(c)
			var body struct {
				Data TenantExperience `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if body.Data.Branding.AppName != tt.want {
				t.Fatalf("app_name = %q, want %q", body.Data.Branding.AppName, tt.want)
			}
		})
	}
}
