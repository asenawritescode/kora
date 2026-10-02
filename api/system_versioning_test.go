package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
)

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
