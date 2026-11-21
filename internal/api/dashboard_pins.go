package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// maxDashboardPins bounds the tiles one dashboard of a shop may pin.
const maxDashboardPins = 4

var (
	errPinLimit    = errors.New("dashboard already holds the maximum number of pins")
	errPinNotFound = errors.New("dashboard pin not found")
)

// addPin answers POST /dashboards/:name/pins. Only the administrator role pins a question. The question is
// checked the way a saved report is, and the administrator must be able to read every record type it uses.
func (d dashboardsAPI) addPin(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	req, body, ok := pinRequestOf(c)
	if !ok {
		return
	}
	label := strings.TrimSpace(req.Label)
	name := c.Param("name")
	store, dashboards, ok := d.writeTarget(c)
	if !ok {
		return
	}
	if _, found := findDashboard(dashboards, name); !found {
		notFoundError(c, "dashboards.not_found", fmt.Sprintf("dashboard %q not found", name), nil)
		return
	}
	cat, err := d.catalog(c)
	if err != nil {
		internalError(c, "loading metric definitions", err)
		return
	}
	models, err := checkSavedQuestion(cat, questionJoinSet(c, d.registry), req.Question)
	if err != nil {
		writeError(c, http.StatusUnprocessableEntity, "dashboards.pin_invalid", err.Error(), nil)
		return
	}
	if !d.canRead(c, models) {
		writeError(c, http.StatusForbidden, "dashboards.forbidden", "You cannot read every record type this pin uses.", nil)
		return
	}
	pin := contract.DashboardPin{ID: contract.NewEventID(), Label: label, Question: req.Question}
	sum := sha256.Sum256(body)
	audit := overrideAudit{actor: actorOf(c), payloadHash: hex.EncodeToString(sum[:])}
	add := func(pins []contract.DashboardPin) ([]contract.DashboardPin, error) { return withPin(pins, pin) }
	if err := store.changePins(c.Request.Context(), name, "dashboard.pin.add", audit, add); err != nil {
		pinError(c, err)
		return
	}
	c.JSON(http.StatusCreated, Response{Data: pin})
}

// pinRequestOf reads and checks a pin request body. It returns the raw body for the audit digest, and writes
// the refusal and reports false when the body is not JSON or the label is blank or too long.
func pinRequestOf(c *gin.Context) (contract.DashboardPinRequest, []byte, bool) {
	var req contract.DashboardPinRequest
	body, err := c.GetRawData()
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid dashboard pin", nil)
		return req, nil, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid dashboard pin", nil)
		return req, nil, false
	}
	label := strings.TrimSpace(req.Label)
	if label == "" || utf8.RuneCountInString(label) > maxReportName {
		writeError(c, http.StatusUnprocessableEntity, "dashboards.pin_invalid", fmt.Sprintf("A pin label is 1 to %d characters.", maxReportName), nil)
		return req, nil, false
	}
	return req, body, true
}

// removePin answers DELETE /dashboards/:name/pins/:id. Only the administrator role unpins a question.
func (d dashboardsAPI) removePin(c *gin.Context) {
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
	id := c.Param("id")
	remove := func(pins []contract.DashboardPin) ([]contract.DashboardPin, error) { return withoutPin(pins, id) }
	if err := store.changePins(c.Request.Context(), name, "dashboard.pin.remove", overrideAudit{actor: actorOf(c)}, remove); err != nil {
		pinError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// pinError writes the response for a failed pin change: 409 at the limit, 404 for an unknown pin, 500 otherwise.
func pinError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errPinLimit):
		conflictError(c, "dashboards.pin_limit", fmt.Sprintf("A dashboard holds at most %d pins.", maxDashboardPins), nil)
	case errors.Is(err, errPinNotFound):
		notFoundError(c, "dashboards.pin_not_found", "Dashboard pin not found", nil)
	default:
		internalError(c, "changing dashboard pins", err)
	}
}

// visiblePins returns the pins the caller may see: those whose question still checks against the catalog and
// whose record types the caller can all read. A pin the caller cannot read is left out, not refused.
func (d dashboardsAPI) visiblePins(c *gin.Context, cat metrics.Catalog, pins []contract.DashboardPin) []contract.DashboardPin {
	out := []contract.DashboardPin{}
	if len(pins) == 0 {
		return out
	}
	join := questionJoinSet(c, d.registry)
	for _, pin := range pins {
		if models, err := checkSavedQuestion(cat, join, pin.Question); err == nil && d.canRead(c, models) {
			out = append(out, pin)
		}
	}
	return out
}

// canRead reports whether the caller may read every named record type.
func (d dashboardsAPI) canRead(c *gin.Context, models []string) bool {
	_, denial := metricScope(c, analyticsRegistry(c, d.registry), models)
	return denial == nil
}

// questionJoinSet is the set of record types the caller may join to, as a saved question is checked.
func questionJoinSet(c *gin.Context, fallback *doctype.Registry) map[string]bool {
	join := map[string]bool{}
	if reg := analyticsRegistry(c, fallback); reg != nil {
		_, join = exploreAccess(c, reg, reg.All())
	}
	return join
}

// decodePins reads the pinned column. An empty value is no pins, which is how a column added to an existing
// table reads before its first pin.
func decodePins(raw string) ([]contract.DashboardPin, error) {
	if raw == "" {
		return nil, nil
	}
	var pins []contract.DashboardPin
	if err := json.Unmarshal([]byte(raw), &pins); err != nil {
		return nil, err
	}
	return pins, nil
}

// encodePins renders pins for the pinned column, as a JSON array even when there are none.
func encodePins(pins []contract.DashboardPin) (string, error) {
	if pins == nil {
		pins = []contract.DashboardPin{}
	}
	raw, err := json.Marshal(pins)
	return string(raw), err
}

// withPin returns pins with pin appended. A dashboard at the limit refuses it.
func withPin(pins []contract.DashboardPin, pin contract.DashboardPin) ([]contract.DashboardPin, error) {
	if len(pins) >= maxDashboardPins {
		return nil, errPinLimit
	}
	return append(slices.Clone(pins), pin), nil
}

// withoutPin returns pins without the pin named id. An unknown id is refused.
func withoutPin(pins []contract.DashboardPin, id string) ([]contract.DashboardPin, error) {
	i := slices.IndexFunc(pins, func(p contract.DashboardPin) bool { return p.ID == id })
	if i < 0 {
		return nil, errPinNotFound
	}
	return slices.Delete(slices.Clone(pins), i, i+1), nil
}

// changePins reads the pins of one dashboard under a row lock, applies change and writes the result, with
// its audit entry, in one transaction. A refused change writes nothing.
func (s overrideStore) changePins(ctx context.Context, name, command string, audit overrideAudit, change func([]contract.DashboardPin) ([]contract.DashboardPin, error)) error {
	return s.withAudit(ctx, command, name, audit, func(tx *sql.Tx) error {
		ensure := db.Rebind(s.dialect, `INSERT INTO _kora_dashboard_override (name, hidden, display_order, default_range, pinned, modified, modified_by) VALUES (?, '[]', '[]', '', '[]', ?, ?) `+
			s.dialect.UpsertClause([]string{"name"}, []string{"name"}))
		if _, err := tx.ExecContext(ctx, ensure, name, time.Now().UTC(), audit.actor.SubjectUserID); err != nil {
			return err
		}
		var raw string
		lock := db.Rebind(s.dialect, `SELECT pinned FROM _kora_dashboard_override WHERE name = ?`) + s.dialect.ExclusiveRowLock()
		if err := tx.QueryRowContext(ctx, lock, name).Scan(&raw); err != nil {
			return err
		}
		pins, err := decodePins(raw)
		if err != nil {
			return err
		}
		next, err := change(pins)
		if err != nil {
			return err
		}
		encoded, err := encodePins(next)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_dashboard_override SET pinned = ?, modified = ?, modified_by = ? WHERE name = ?`),
			encoded, time.Now().UTC(), audit.actor.SubjectUserID, name)
		return err
	})
}
