package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/doctype"
	"github.com/gin-gonic/gin"
)

func TestSiteSettingsExposeCurrencyPrecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name      string
		stored    *string
		currency  string
		precision float64
	}{
		{"default", nil, "KES", 2},
		{"zero decimal currency", strPtr("UGX"), "UGX", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			query := mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WithArgs("s1", "currency")
			if tt.stored == nil {
				query.WillReturnRows(sqlmock.NewRows([]string{"setting_value"}))
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"setting_value"}).AddRow(*tt.stored))
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/system/settings", nil)
			c.Set("site_db", db)
			c.Set("site_name", "s1")
			c.Set("site_registry", doctype.NewRegistry())
			NewHandler(doctype.NewRegistry(), nil).HandleSiteSettings(c)

			var body struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if body.Data["currency"] != tt.currency || body.Data["currency_precision"] != tt.precision {
				t.Fatalf("settings = %#v", body.Data)
			}
		})
	}
}

func TestDecimalSchemaMetadata(t *testing.T) {
	two := 2
	schema := FieldToJSONSchema(&doctype.Field{Fieldname: "total", Fieldtype: "Currency", Options: "currency", Precision: &two})
	if schema["type"] != "number" || schema["format"] != "decimal" || schema["x-kora-precision"] != 2 || schema["x-kora-currency-field"] != "currency" {
		t.Fatalf("schema = %#v", schema)
	}
}

func strPtr(s string) *string { return &s }
