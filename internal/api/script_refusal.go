package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

// parseVersionSnapshot parses a stored version's config for activation or rollback
// and runs the script planning check on it. The check runs before any DDL or journal
// row. A parse failure answers 500, a refused script 409; ok is false when the
// request was answered.
func parseVersionSnapshot(c *gin.Context, database *sql.DB, dialect kdb.Dialect, configJSON string) (snapshot *doctype.ConfigSnapshot, ok bool) {
	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to parse version config: " + err.Error()},
		})
		return nil, false
	}
	err = configstore.CheckSnapshotScripts(database, dialect, contract.TenantID(c.GetString("site_id")), snapshot.Scripts)
	if err == nil {
		return snapshot, true
	}
	var refusal *configstore.ScriptRefusal
	if errors.As(err, &refusal) {
		writeError(c, http.StatusConflict, refusal.Code, refusal.Error(), nil)
		return nil, false
	}
	internalError(c, "checking config script rows", err)
	return nil, false
}
