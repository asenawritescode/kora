//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/doctype"
)

// adHocQuestion is "sum of base amounts of Sale records, Paid only, last week": a question of an unsaved measure.
const adHocQuestion = `{"ad_hoc":{"record_type":"Sale","agg":"sum","field":"base_total","filters":[{"field":"status","op":"=","value":"Paid"}]},"period":"last_week","grain":"range"}`

// TestAdHocSavedQuestionSaveAndPinAcrossDialects saves an ad-hoc measure question as a shop report and pins it to
// Shop insights, on MySQL and PostgreSQL. The administrator may do both; a cashier may save nothing for the shop and
// pin nothing. A question naming an unknown field is refused on save and on pin (422, as the other question validation).
func TestAdHocSavedQuestionSaveAndPinAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_AD_HOC_SAVED_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_AD_HOC_SAVED_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			captureLogs(t)
			database, reg := openErpsell(t, tc.dialect, dsn, nil, nil)
			router := askZanaRouter(t, database, reg, tc.dialect)

			saved := postAs(t, router, goldenAdminUser, doctype.AdminRole, "/api/v1/explore/reports",
				`{"name":"Paid sales last week","visibility":"shop","question":`+adHocQuestion+`}`, http.StatusCreated)
			if !strings.Contains(saved.Body.String(), `"ad_hoc"`) {
				t.Fatalf("saved report lost its ad-hoc measure: %s", saved.Body)
			}
			postAs(t, router, goldenAdminUser, doctype.AdminRole, "/api/v1/explore/reports",
				`{"name":"Unknown field","visibility":"private","question":{"ad_hoc":{"record_type":"Sale","agg":"sum","field":"no_such_field"},"period":"last_week","grain":"range"}}`,
				http.StatusUnprocessableEntity)

			postAs(t, router, goldenAdminUser, doctype.AdminRole, "/api/v1/dashboards/shop_insights/pins",
				`{"label":"Paid sales last week","question":`+adHocQuestion+`}`, http.StatusCreated)
			recorder := doAs(t, router, goldenAdminUser, doctype.AdminRole, "/api/v1/dashboards/shop_insights", "")
			if !strings.Contains(recorder.Body.String(), "Paid sales last week") {
				t.Fatalf("pinned tile is missing from Shop insights: %s", recorder.Body)
			}
			postAs(t, router, "alice", "Cashier", "/api/v1/dashboards/shop_insights/pins",
				`{"label":"Not mine","question":`+adHocQuestion+`}`, http.StatusForbidden)
			postAs(t, router, goldenAdminUser, doctype.AdminRole, "/api/v1/dashboards/shop_insights/pins",
				`{"label":"Unknown","question":{"ad_hoc":{"record_type":"Sale","agg":"sum","field":"no_such_field"}}}`, http.StatusUnprocessableEntity)
		})
	}
}

// postAs sends one POST as user with role, checks the status and returns the answer.
func postAs(t *testing.T, router *gin.Engine, user, role, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User", user)
	req.Header.Set("X-Roles", role)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != want {
		t.Fatalf("POST %s as %s/%s = %d, want %d: %s", path, user, role, recorder.Code, want, recorder.Body)
	}
	var env json.RawMessage
	_ = json.Unmarshal(recorder.Body.Bytes(), &env)
	return recorder
}
