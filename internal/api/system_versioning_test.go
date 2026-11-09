package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/gin-gonic/gin"
)

func TestConfigVersionDiffIncludesModifiedRolesAndPermissions(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: &kdb.PostgresDialect{}})
	from := doctype.ToSExpr(&doctype.ConfigSnapshot{
		Roles:       []*doctype.Role{{Name: "Shop Staff", WorkspaceAccess: true}},
		Permissions: []*doctype.Permission{{Doctype: "Stock Count", Role: "Shop Staff", Read: true}},
	})
	to := doctype.ToSExpr(&doctype.ConfigSnapshot{
		Roles:       []*doctype.Role{{Name: "Shop Staff", WorkspaceAccess: true, Description: "Prepare draft counts"}},
		Permissions: []*doctype.Permission{{Doctype: "Stock Count", Role: "Shop Staff", Read: true, Write: true}},
	})
	mock.ExpectQuery(`SELECT config FROM _kora_config_version WHERE id = \$1`).WithArgs("from-1").
		WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow(from))
	mock.ExpectQuery(`SELECT config FROM _kora_config_version WHERE id = \$1`).WithArgs("to-2").
		WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow(to))

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/system/config/diff?from=from-1&to=to-2", nil)
	handler.HandleConfigDiff(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("config diff returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data struct {
			SectionChanges []doctype.SectionChange `json:"section_changes"`
			Summary        string                  `json:"diff_summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal("decode diff response:", err)
	}
	if got, want := len(response.Data.SectionChanges), 2; got != want {
		t.Fatalf("section changes = %d, want %d: %s", got, want, recorder.Body.String())
	}
	if response.Data.Summary == "0 added, 0 removed, 0 changed (0 breaking)" {
		t.Fatalf("summary hides the role and permission changes: %s", recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}

func TestConfigVersionPreviewUsesBaseSnapshotForFullDiff(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: &kdb.PostgresDialect{}})
	base := doctype.ToSExpr(&doctype.ConfigSnapshot{Roles: []*doctype.Role{{Name: "Shop Staff", WorkspaceAccess: true}}})
	draft := doctype.ToSExpr(&doctype.ConfigSnapshot{
		Roles: []*doctype.Role{{Name: "Shop Staff", WorkspaceAccess: true, Description: "Prepare draft counts"}},
	})
	mock.ExpectQuery(`SELECT config, site, status, COALESCE\(base_version_id, ''\), version FROM _kora_config_version WHERE id = \$1`).
		WithArgs("draft-2").WillReturnRows(sqlmock.NewRows([]string{"config", "site", "status", "base_version_id", "version"}).
		AddRow(draft, "site-1", "Draft", "base-1", 2))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_config_version WHERE site = \$1 AND version > \(SELECT version FROM _kora_config_version WHERE id = \$2\) AND status = 'Active'`).
		WithArgs("site-1", "draft-2").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT config FROM _kora_config_version WHERE id = \$1 AND site = \$2`).
		WithArgs("base-1", "site-1").WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow(base))

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/system/config/versions/draft-2/preview", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "draft-2"}}
	handler.HandleConfigVersionPreview(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("config preview returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data struct {
			DiffSummary    string                  `json:"diff_summary"`
			SectionChanges []doctype.SectionChange `json:"section_changes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal("decode preview response:", err)
	}
	if len(response.Data.SectionChanges) != 1 || response.Data.DiffSummary == "0 added, 0 removed, 0 changed (0 breaking)" {
		t.Fatalf("preview omitted full snapshot changes: %s", recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}

func TestIsStaleBaseVersion(t *testing.T) {
	tests := []struct {
		name             string
		baseVersionID    string
		baseConfigHash   string
		activeVersionID  string
		activeConfigHash string
		want             bool
	}{
		{
			name:             "same hash different ids is not stale",
			baseVersionID:    "cv-1",
			baseConfigHash:   "abc",
			activeVersionID:  "cv-2",
			activeConfigHash: "abc",
			want:             false,
		},
		{
			name:             "different hash is stale",
			baseVersionID:    "cv-1",
			baseConfigHash:   "abc",
			activeVersionID:  "cv-2",
			activeConfigHash: "def",
			want:             true,
		},
		{
			name:            "fallback to id comparison when hashes missing",
			baseVersionID:   "cv-1",
			activeVersionID: "cv-2",
			want:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isStaleBaseVersion(tt.baseVersionID, tt.baseConfigHash, tt.activeVersionID, tt.activeConfigHash)
			if got != tt.want {
				t.Fatalf("isStaleBaseVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigVersionActivateFailsClosedWhenActiveVersionCannotBeRead(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: &kdb.PostgresDialect{}})
	mock.ExpectQuery(`SELECT config, site, status, COALESCE\(change_list, ''\), COALESCE\(base_version_id, ''\), COALESCE\(min_kora_version, ''\) FROM _kora_config_version WHERE id = \$1`).
		WithArgs("draft-1").
		WillReturnRows(sqlmock.NewRows([]string{"config", "site", "status", "change_list", "base_version_id", "min_kora_version"}).
			AddRow("", "site-1", "Draft", "", "base-1", ""))
	mock.ExpectQuery(`SELECT id, COALESCE\(config_hash, ''\) FROM _kora_config_version WHERE site = \$1 AND status = 'Active' ORDER BY version DESC LIMIT 1`).
		WithArgs("site-1").
		WillReturnError(sql.ErrConnDone)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/versions/draft-1/activate", strings.NewReader(""))
	ctx.Params = gin.Params{{Key: "id", Value: "draft-1"}}
	handler.HandleConfigVersionActivate(ctx)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("activation with unreadable active version returned HTTP %d, want 500: %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}

func TestConfigVersionActivateRejectsMissingDraftBase(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: &kdb.PostgresDialect{}})
	mock.ExpectQuery(`SELECT config, site, status, COALESCE\(change_list, ''\), COALESCE\(base_version_id, ''\), COALESCE\(min_kora_version, ''\) FROM _kora_config_version WHERE id = \$1`).
		WithArgs("draft-2").
		WillReturnRows(sqlmock.NewRows([]string{"config", "site", "status", "change_list", "base_version_id", "min_kora_version"}).
			AddRow("", "site-2", "Draft", "", "deleted-base", ""))
	mock.ExpectQuery(`SELECT id, COALESCE\(config_hash, ''\) FROM _kora_config_version WHERE site = \$1 AND status = 'Active' ORDER BY version DESC LIMIT 1`).
		WithArgs("site-2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "config_hash"}).AddRow("active-2", "hash-active"))
	mock.ExpectQuery(`SELECT COALESCE\(config_hash, ''\) FROM _kora_config_version WHERE id = \$1`).
		WithArgs("deleted-base").
		WillReturnError(sql.ErrNoRows)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/versions/draft-2/activate", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "draft-2"}}
	handler.HandleConfigVersionActivate(ctx)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("activation with a missing draft base returned HTTP %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}
