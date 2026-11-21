package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/metrics"
)

// maxDashboardKeyLen bounds one tile metric or chart id named by an override.
const maxDashboardKeyLen = 140

// saveOverride answers PUT /dashboards/:name/override. The body replaces the tenant's override of the
// dashboard. Only the administrator role may change a layout, and the change is audited.
func (d dashboardsAPI) saveOverride(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	body, err := c.GetRawData()
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid dashboard override", nil)
		return
	}
	var o contract.DashboardOverride
	if err := json.Unmarshal(body, &o); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid dashboard override", nil)
		return
	}
	if err := validateDashboardOverride(o); err != nil {
		writeError(c, http.StatusBadRequest, "dashboards.override_invalid", err.Error(), nil)
		return
	}
	o.Hidden, o.Order = nonNil(o.Hidden), nonNil(o.Order)
	name := c.Param("name")
	store, dashboards, ok := d.writeTarget(c)
	if !ok {
		return
	}
	if _, found := findDashboard(dashboards, name); !found {
		notFoundError(c, "dashboards.not_found", fmt.Sprintf("dashboard %q not found", name), nil)
		return
	}
	sum := sha256.Sum256(body)
	audit := overrideAudit{actor: actorOf(c), payloadHash: hex.EncodeToString(sum[:])}
	if err := store.save(c.Request.Context(), name, o, audit); err != nil {
		internalError(c, "saving dashboard override", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: o})
}

// resetOverride answers DELETE /dashboards/:name/override, which restores the pack's layout. Only the
// administrator role may reset a layout, and the reset is audited.
func (d dashboardsAPI) resetOverride(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	name := c.Param("name")
	store, dashboards, ok := d.writeTarget(c)
	if !ok {
		return
	}
	if _, found := findDashboard(dashboards, name); !found {
		notFoundError(c, "dashboards.not_found", fmt.Sprintf("dashboard %q not found", name), nil)
		return
	}
	if err := store.reset(c.Request.Context(), name, overrideAudit{actor: actorOf(c)}); err != nil {
		internalError(c, "resetting dashboard override", err)
		return
	}
	c.Status(http.StatusNoContent)
}

// writeTarget returns the override store of the request's site with the site's dashboard definitions. It
// writes the error response and reports false when the site cannot take a write.
func (d dashboardsAPI) writeTarget(c *gin.Context) (overrideStore, []metrics.Dashboard, bool) {
	_, dashboards, _, ok := d.load(c)
	if !ok {
		return overrideStore{}, nil, false
	}
	store := overrideStore{database: getSiteDB(c, d.siteDB), dialect: siteDialectFromContext(c, d.dialect)}
	return store, dashboards, true
}

// validateDashboardOverride checks that every name is non-blank, bounded and listed once, and that the
// default range is one dashboards use.
func validateDashboardOverride(o contract.DashboardOverride) error {
	if o.DefaultRange != "" && !metrics.ValidDashboardRange(o.DefaultRange) {
		return fmt.Errorf("default_range %q is not a dashboard range", o.DefaultRange)
	}
	for _, names := range [][]string{o.Hidden, o.Order} {
		seen := map[string]bool{}
		for _, name := range names {
			if strings.TrimSpace(name) == "" || len(name) > maxDashboardKeyLen {
				return fmt.Errorf("dashboard names must be 1 to %d characters", maxDashboardKeyLen)
			}
			if seen[name] {
				return fmt.Errorf("dashboard name %q is listed more than once", name)
			}
			seen[name] = true
		}
	}
	return nil
}

// nonNil returns list, or an empty list when it is nil, so the override stores as a JSON array.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// actorOf is the verified caller of a request that passed requireAdmin.
func actorOf(c *gin.Context) contract.ActorContext {
	execution, _ := requestExecution(c)
	return execution.Actor()
}

// overrideAudit is who changed an override and the digest of what they sent.
type overrideAudit struct {
	actor       contract.ActorContext
	payloadHash string
}

// readDashboardOverrides returns every stored override by dashboard name.
func readDashboardOverrides(ctx context.Context, database *sql.DB) (map[string]contract.DashboardOverride, error) {
	rows, err := database.QueryContext(ctx, `SELECT name, hidden, display_order, default_range FROM _kora_dashboard_override`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only query; a close error after a full read changes nothing
	out := map[string]contract.DashboardOverride{}
	for rows.Next() {
		var name, hidden, order string
		var o contract.DashboardOverride
		if err := rows.Scan(&name, &hidden, &order, &o.DefaultRange); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(hidden), &o.Hidden); err != nil {
			return nil, fmt.Errorf("decode hidden of dashboard %q: %w", name, err)
		}
		if err := json.Unmarshal([]byte(order), &o.Order); err != nil {
			return nil, fmt.Errorf("decode order of dashboard %q: %w", name, err)
		}
		out[name] = o
	}
	return out, rows.Err()
}

// overrideStore writes dashboard overrides to one site database, each change together with its audit entry.
type overrideStore struct {
	database *sql.DB
	dialect  db.Dialect
}

// save replaces the override of one dashboard. The row and its audit entry commit together.
func (s overrideStore) save(ctx context.Context, name string, o contract.DashboardOverride, audit overrideAudit) error {
	hidden, err := json.Marshal(o.Hidden)
	if err != nil {
		return err
	}
	order, err := json.Marshal(o.Order)
	if err != nil {
		return err
	}
	query := db.Rebind(s.dialect, `INSERT INTO _kora_dashboard_override (name, hidden, display_order, default_range, modified, modified_by) VALUES (?, ?, ?, ?, ?, ?) `+
		s.dialect.UpsertClause([]string{"name"}, []string{"hidden", "display_order", "default_range", "modified", "modified_by"}))
	return s.withAudit(ctx, "dashboard.override.save", name, audit, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, name, string(hidden), string(order), o.DefaultRange, time.Now().UTC(), audit.actor.SubjectUserID)
		return err
	})
}

// reset removes the override of one dashboard. Resetting a dashboard without an override succeeds, and the
// reset is audited either way.
func (s overrideStore) reset(ctx context.Context, name string, audit overrideAudit) error {
	return s.withAudit(ctx, "dashboard.override.reset", name, audit, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_dashboard_override WHERE name = ?`), name)
		return err
	})
}

// withAudit runs write and the audit entry for it in one transaction.
func (s overrideStore) withAudit(ctx context.Context, command, name string, audit overrideAudit, write func(*sql.Tx) error) error {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if err := write(tx); err != nil {
		return err
	}
	if err := s.insertAudit(ctx, tx, command, name, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// insertAudit writes the operation audit entry for an override change, in the caller's transaction.
func (s overrideStore) insertAudit(ctx context.Context, tx *sql.Tx, command, name string, audit overrideAudit) error {
	roles, err := json.Marshal(audit.actor.Roles)
	if err != nil {
		return err
	}
	actorUser := audit.actor.SubjectUserID
	if actorUser == "" {
		actorUser = audit.actor.PrincipalID
	}
	_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_operation_audit
		(id, site, operation_id, source, principal_type, principal_id, actor_user, actor_roles, command_name, doctype, doc_name, status, payload_hash)
		VALUES (?, ?, ?, 'api', ?, ?, ?, ?, ?, '_kora_dashboard_override', ?, 'completed', ?)`),
		contract.NewEventID(), audit.actor.Site, contract.NewOperationID(), string(audit.actor.PrincipalType),
		audit.actor.PrincipalID, actorUser, string(roles), command, name, audit.payloadHash)
	return err
}
