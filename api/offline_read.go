package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

const offlinePageLimit = 100
const offlineSnapshotLifetime = 24 * time.Hour
const offlineCursorLifetime = 30 * 24 * time.Hour

// HandleOfflineCapabilities lets a client pin metadata and a snapshot to one
// site configuration generation before declaring its download complete.
func (h *Handler) HandleOfflineCapabilities(c *gin.Context) {
	if _, _, _, ok := offlineIdentity(c); !ok {
		h.writeKernelError(c, unauthenticatedOffline())
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{
		"protocol_version":          1,
		"generation":                c.GetUint64("site_config_revision"),
		"max_page_size":             offlinePageLimit,
		"snapshot_lifetime_seconds": int(offlineSnapshotLifetime.Seconds()),
		"cursor_lifetime_seconds":   int(offlineCursorLifetime.Seconds()),
	}})
}

type offlineDocument = contract.OfflineDocumentV1
type offlineMarker = contract.OfflineTombstoneV1
type offlineBootstrapRequest = contract.OfflineBootstrapRequestV1
type offlineBootstrapResponse = contract.OfflineBootstrapResponseV1
type offlinePullRequest = contract.OfflinePullRequestV1
type offlinePullResponse = contract.OfflinePullResponseV1

type offlineTokenTypes struct {
	Doctypes      []string               `json:"doctypes"`
	BusinessScope contract.BusinessScope `json:"business_scope"`
}

func offlineProject(doctypeName string, data map[string]any) offlineDocument {
	raw, _ := json.Marshal(data)
	return offlineDocument{Doctype: doctypeName, Record: raw}
}

func offlineToken() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
func offlineIdentity(c *gin.Context) (string, string, *sql.DB, bool) {
	execution, verified := requestExecution(c)
	value, exists := c.Get("site_db")
	database, ok := value.(*sql.DB)
	if !verified || !exists || !ok || database == nil || c.GetString("site_name") == "" {
		return "", "", nil, false
	}
	actor := execution.Actor().SubjectUserID
	if actor == "" {
		actor = execution.Actor().PrincipalID
	}
	return c.GetString("site_name"), actor, database, true
}
func unauthenticatedOffline() *contract.Error {
	return contract.NewError(contract.CodeUnauthenticated, "verified execution identity is required")
}
func (h *Handler) offlineSQL(c *gin.Context, query string) string {
	return kdb.Rebind(h.siteDialect(c), query)
}
func offlineLimit(value int) int {
	if value < 1 {
		return offlinePageLimit
	}
	if value > offlinePageLimit {
		return offlinePageLimit
	}
	return value
}

// HandleOfflineBootstrap creates an immutable, actor-scoped snapshot. A single
// repeatable-read transaction observes the outbox boundary and every selected
// record; materialized pages survive process restarts until token expiry.
func (h *Handler) HandleOfflineBootstrap(c *gin.Context) {
	var req offlineBootstrapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequestError(c, "offline.invalid_request", "Invalid bootstrap request", nil)
		return
	}
	site, actor, database, ok := offlineIdentity(c)
	if !ok {
		h.writeKernelError(c, unauthenticatedOffline())
		return
	}
	if req.SnapshotToken == "" {
		if len(req.Doctypes) > 100 {
			badRequestError(c, "offline.invalid_selection", "Select at most 100 DocTypes", nil)
			return
		}
		h.createOfflineSnapshot(c, site, actor, database, req)
		return
	}
	h.pageOfflineSnapshot(c, site, actor, database, req)
}

func (h *Handler) createOfflineSnapshot(c *gin.Context, site, actor string, database *sql.DB, req offlineBootstrapRequest) {
	// Reap expired materialized pages and cursors before allocating another
	// snapshot. A failed cleanup does not compromise an active download.
	now := time.Now().UTC()
	_, _ = database.ExecContext(c.Request.Context(), h.offlineSQL(c, "DELETE FROM _kora_offline_snapshot_row WHERE token IN (SELECT token FROM _kora_offline_snapshot WHERE expires_at < ?)"), now)
	_, _ = database.ExecContext(c.Request.Context(), h.offlineSQL(c, "DELETE FROM _kora_offline_snapshot WHERE expires_at < ?"), now)
	_, _ = database.ExecContext(c.Request.Context(), h.offlineSQL(c, "DELETE FROM _kora_offline_cursor WHERE expires_at < ?"), now)
	registry := h.siteRegistry(c)
	selected := make([]*doctype.DocType, 0, len(req.Doctypes))
	scopes := make(map[string]any, len(req.Doctypes))
	var businessScope contract.BusinessScope
	seen := map[string]bool{}
	for _, name := range req.Doctypes {
		dt := registry.Get(name)
		if dt == nil {
			badRequestError(c, "offline.unknown_doctype", "Selected DocType is unavailable", nil)
			return
		}
		if seen[dt.Name] {
			continue
		}
		scope, forbidden := checkPerm(c, registry, dt.Name, "read")
		if forbidden {
			return
		}
		seen[dt.Name] = true
		selected = append(selected, dt)
		scopes[dt.Name] = scope
		businessScope = scope.Business
	}
	tx, err := database.BeginTx(c.Request.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		internalError(c, "offline snapshot unavailable", err)
		return
	}
	defer tx.Rollback()
	var boundary int64
	if err := tx.QueryRowContext(c.Request.Context(), h.offlineSQL(c, "SELECT COALESCE(MAX(source_sequence),0) FROM _kora_outbox WHERE site = ?"), site).Scan(&boundary); err != nil {
		internalError(c, "offline snapshot boundary unavailable", err)
		return
	}
	reader := h.siteTx(c)
	reader.Transaction = tx
	rows := make([]offlineDocument, 0)
	names := make([]string, 0, len(selected))
	for _, dt := range selected {
		names = append(names, dt.Name)
		scope := scopes[dt.Name].(policy.RowScope)
		for offset := 0; ; offset += offlinePageLimit {
			docs, _, err := reader.GetListWithScope(dt, "", "name asc", offlinePageLimit, offset, scope, nil, "")
			if err != nil {
				internalError(c, "offline snapshot read failed", err)
				return
			}
			for _, summary := range docs {
				doc, err := reader.GetDocWithScope(dt, summary.Name, scope)
				if err != nil {
					internalError(c, "offline snapshot detail failed", err)
					return
				}
				rows = append(rows, offlineProject(dt.Name, docToMap(doc, dt, registry, nil)))
			}
			if len(docs) < offlinePageLimit {
				break
			}
		}
	}
	if err := tx.Commit(); err != nil {
		internalError(c, "offline snapshot failed", err)
		return
	}
	token, err := offlineToken()
	if err != nil {
		internalError(c, "offline snapshot token failed", err)
		return
	}
	write, err := database.BeginTx(c.Request.Context(), nil)
	if err != nil {
		internalError(c, "offline snapshot unavailable", err)
		return
	}
	defer write.Rollback()
	encodedTypes, _ := json.Marshal(offlineTokenTypes{Doctypes: names, BusinessScope: businessScope})
	expires := time.Now().UTC().Add(offlineSnapshotLifetime)
	_, err = write.ExecContext(c.Request.Context(), h.offlineSQL(c, "INSERT INTO _kora_offline_snapshot (token,site,actor,generation,boundary_sequence,types_json,expires_at) VALUES (?,?,?,?,?,?,?)"), token, site, actor, c.GetUint64("site_config_revision"), boundary, string(encodedTypes), expires)
	if err != nil {
		internalError(c, "offline snapshot storage failed", err)
		return
	}
	for i, row := range rows {
		if _, err := write.ExecContext(c.Request.Context(), h.offlineSQL(c, "INSERT INTO _kora_offline_snapshot_row (token,row_index,doctype,document) VALUES (?,?,?,?)"), token, i, row.Doctype, string(row.Record)); err != nil {
			internalError(c, "offline snapshot storage failed", err)
			return
		}
	}
	if err := write.Commit(); err != nil {
		internalError(c, "offline snapshot storage failed", err)
		return
	}
	h.pageOfflineSnapshot(c, site, actor, database, offlineBootstrapRequest{SnapshotToken: token, Limit: req.Limit})
}

func (h *Handler) pageOfflineSnapshot(c *gin.Context, site, actor string, database *sql.DB, req offlineBootstrapRequest) {
	var boundSite, boundActor, typesJSON string
	var generation uint64
	var boundary int64
	var expires time.Time
	err := database.QueryRowContext(c.Request.Context(), h.offlineSQL(c, "SELECT site,actor,generation,boundary_sequence,types_json,expires_at FROM _kora_offline_snapshot WHERE token = ?"), req.SnapshotToken).Scan(&boundSite, &boundActor, &generation, &boundary, &typesJSON, &expires)
	if err != nil || boundSite != site || boundActor != actor || time.Now().After(expires) || generation != c.GetUint64("site_config_revision") {
		conflictError(c, "offline.rebootstrap_required", "Download the workspace again", nil)
		return
	}
	page := 0
	if req.PageToken != "" {
		page, err = strconv.Atoi(req.PageToken)
		if err != nil || page < 0 {
			badRequestError(c, "offline.invalid_page", "Invalid bootstrap page", nil)
			return
		}
	}
	rows, err := database.QueryContext(c.Request.Context(), h.offlineSQL(c, "SELECT doctype,document FROM _kora_offline_snapshot_row WHERE token = ? AND row_index >= ? ORDER BY row_index LIMIT ?"), req.SnapshotToken, page, offlineLimit(req.Limit)+1)
	if err != nil {
		internalError(c, "offline snapshot page failed", err)
		return
	}
	defer rows.Close()
	items := make([]offlineDocument, 0)
	for rows.Next() {
		var dt, raw string
		if err := rows.Scan(&dt, &raw); err != nil {
			internalError(c, "offline snapshot page failed", err)
			return
		}
		if !json.Valid([]byte(raw)) {
			internalError(c, "offline snapshot page failed", fmt.Errorf("invalid materialized record JSON"))
			return
		}
		items = append(items, offlineDocument{Doctype: dt, Record: json.RawMessage(raw)})
	}
	if err := rows.Err(); err != nil {
		internalError(c, "offline snapshot page failed", err)
		return
	}
	hasMore := len(items) > offlineLimit(req.Limit)
	if hasMore {
		items = items[:offlineLimit(req.Limit)]
	}
	var tokenTypes offlineTokenTypes
	_ = json.Unmarshal([]byte(typesJSON), &tokenTypes)
	types := tokenTypes.Doctypes
	execution, _ := requestExecution(c)
	if tokenTypes.BusinessScope.Validate() != nil || tokenTypes.BusinessScope != businessScopeFromRequest(c, execution) {
		conflictError(c, "offline.rebootstrap_required", "Workspace scope changed. Download it again", nil)
		return
	}
	for _, name := range types {
		dt := h.siteRegistry(c).Get(name)
		if dt == nil {
			conflictError(c, "offline.rebootstrap_required", "Workspace permissions changed. Download it again", nil)
			return
		}
		if _, permissionErr := policy.DecideForBusinessScope(execution, h.siteRegistry(c), name, "read", tokenTypes.BusinessScope, dt.BusinessScopePolicy == contract.ScopeEntityOwned); permissionErr != nil {
			conflictError(c, "offline.rebootstrap_required", "Workspace permissions changed. Download it again", nil)
			return
		}
	}
	response := offlineBootstrapResponse{SnapshotToken: req.SnapshotToken, Generation: generation, Records: items, Doctypes: types, Complete: !hasMore}
	if hasMore {
		response.NextPageToken = strconv.Itoa(page + len(items))
	} else {
		cursor, err := offlineToken()
		if err != nil {
			internalError(c, "offline cursor failed", err)
			return
		}
		_, err = database.ExecContext(c.Request.Context(), h.offlineSQL(c, "INSERT INTO _kora_offline_cursor (token,site,actor,generation,source_sequence,types_json,expires_at) VALUES (?,?,?,?,?,?,?)"), cursor, site, actor, generation, boundary, typesJSON, time.Now().UTC().Add(offlineCursorLifetime))
		if err != nil {
			internalError(c, "offline cursor failed", err)
			return
		}
		response.PullCursor = cursor
	}
	c.JSON(http.StatusOK, Response{Data: response})
}

// HandleOfflinePull advances through the transactional outbox sequence. Every
// change is rechecked against current read permission and row scope. A delete
// or a newly invisible row is a tombstone for this actor.
func (h *Handler) HandleOfflinePull(c *gin.Context) {
	var req offlinePullRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Cursor == "" {
		badRequestError(c, "offline.invalid_cursor", "A pull cursor is required", nil)
		return
	}
	site, actor, database, ok := offlineIdentity(c)
	if !ok {
		h.writeKernelError(c, unauthenticatedOffline())
		return
	}
	var boundSite, boundActor, typesJSON string
	var generation uint64
	var sequence int64
	var expires time.Time
	err := database.QueryRowContext(c.Request.Context(), h.offlineSQL(c, "SELECT site,actor,generation,source_sequence,types_json,expires_at FROM _kora_offline_cursor WHERE token = ?"), req.Cursor).Scan(&boundSite, &boundActor, &generation, &sequence, &typesJSON, &expires)
	if err != nil || boundSite != site || boundActor != actor || time.Now().After(expires) || generation != c.GetUint64("site_config_revision") {
		conflictError(c, "offline.rebootstrap_required", "Download the workspace again", nil)
		return
	}
	var tokenTypes offlineTokenTypes
	if err := json.Unmarshal([]byte(typesJSON), &tokenTypes); err != nil || tokenTypes.BusinessScope.Validate() != nil {
		conflictError(c, "offline.rebootstrap_required", "Download the workspace again", nil)
		return
	}
	types := tokenTypes.Doctypes
	allowed := make(map[string]bool, len(types))
	execution, _ := requestExecution(c)
	if tokenTypes.BusinessScope != businessScopeFromRequest(c, execution) {
		conflictError(c, "offline.rebootstrap_required", "Workspace scope changed. Download it again", nil)
		return
	}
	for _, name := range types {
		dt := h.siteRegistry(c).Get(name)
		if dt == nil {
			conflictError(c, "offline.rebootstrap_required", "Workspace permissions changed. Download it again", nil)
			return
		}
		if _, permissionErr := policy.DecideForBusinessScope(execution, h.siteRegistry(c), name, "read", tokenTypes.BusinessScope, dt.BusinessScopePolicy == contract.ScopeEntityOwned); permissionErr != nil {
			conflictError(c, "offline.rebootstrap_required", "Workspace permissions changed. Download it again", nil)
			return
		}
		allowed[name] = true
	}
	eventTypes := []string{"record.created", "record.updated"}
	for _, name := range types {
		if dt := h.siteRegistry(c).Get(name); dt != nil {
			eventTypes = append(eventTypes, outbox.EventTypeName(dt.Name, eventstream.EventDelete))
		}
	}
	eventPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(eventTypes)), ",")
	query := "SELECT source_sequence,aggregate_type,aggregate_id,event_type,payload FROM _kora_outbox WHERE site = ? AND source_sequence > ? AND event_type IN (" + eventPlaceholders + ") ORDER BY source_sequence LIMIT ?"
	args := make([]any, 0, len(eventTypes)+3)
	args = append(args, site, sequence)
	for _, eventType := range eventTypes {
		args = append(args, eventType)
	}
	args = append(args, offlineLimit(req.Limit)+1)
	rows, err := database.QueryContext(c.Request.Context(), h.offlineSQL(c, query), args...)
	if err != nil {
		internalError(c, "offline changes unavailable", err)
		return
	}
	type event struct {
		seq                 int64
		doctype, name, kind string
		businessScope       contract.BusinessScope
	}
	events := make([]event, 0)
	for rows.Next() {
		var e event
		var payload []byte
		if err := rows.Scan(&e.seq, &e.doctype, &e.name, &e.kind, &payload); err != nil {
			rows.Close()
			internalError(c, "offline changes unavailable", err)
			return
		}
		var envelope struct {
			BusinessScope contract.BusinessScope `json:"business_scope"`
		}
		_ = json.Unmarshal(payload, &envelope)
		e.businessScope = envelope.BusinessScope
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		internalError(c, "offline changes unavailable", err)
		return
	}
	hasMore := len(events) > offlineLimit(req.Limit)
	if hasMore {
		events = events[:offlineLimit(req.Limit)]
	}
	result := offlinePullResponse{Changes: []offlineDocument{}, Tombstones: []offlineMarker{}, NextCursor: req.Cursor, HasMore: hasMore, Generation: generation}
	registry := h.siteRegistry(c)
	for _, e := range events {
		sequence = e.seq
		dt := registry.Get(e.doctype)
		if dt == nil || e.name == "" || !allowed[dt.Name] {
			continue
		}
		if dt.BusinessScopePolicy == contract.ScopeEntityOwned && (e.businessScope.Validate() != nil || e.businessScope != tokenTypes.BusinessScope) {
			continue
		}
		marker := offlineMarker{Doctype: dt.Name, Name: e.name}
		if strings.HasSuffix(e.kind, ".after_delete") {
			result.Tombstones = append(result.Tombstones, marker)
			continue
		}
		businessScope := tokenTypes.BusinessScope
		decision, decisionErr := policy.DecideForBusinessScope(execution, registry, dt.Name, "read", businessScope, dt.BusinessScopePolicy == contract.ScopeEntityOwned)
		if decisionErr != nil {
			conflictError(c, "offline.rebootstrap_required", "Workspace permissions changed. Download it again", nil)
			return
		}
		scope := requestRowScope(c, decision, businessScope)
		doc, err := h.siteTx(c).GetDocWithScope(dt, e.name, scope)
		if errors.Is(err, orm.ErrNotFound) {
			result.Tombstones = append(result.Tombstones, marker)
			continue
		}
		if err != nil {
			internalError(c, "offline change read failed", err)
			return
		}
		result.Changes = append(result.Changes, offlineProject(dt.Name, docToMap(doc, dt, registry, nil)))
	}
	if len(events) > 0 {
		next, err := offlineToken()
		if err != nil {
			internalError(c, "offline cursor failed", err)
			return
		}
		_, err = database.ExecContext(c.Request.Context(), h.offlineSQL(c, "INSERT INTO _kora_offline_cursor (token,site,actor,generation,source_sequence,types_json,expires_at) VALUES (?,?,?,?,?,?,?)"), next, site, actor, generation, sequence, typesJSON, time.Now().UTC().Add(offlineCursorLifetime))
		if err != nil {
			internalError(c, "offline cursor failed", err)
			return
		}
		result.NextCursor = next
	}
	c.JSON(http.StatusOK, Response{Data: result})
}
