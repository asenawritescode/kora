package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
)

// RecordHistoryEntry is the user-facing activity projection for one record.
// It includes only visible field diffs; command payloads and audit hashes stay
// private to the operation ledger.
type RecordHistoryEntry struct {
	Actor     string                     `json:"actor"`
	Action    string                     `json:"action"`
	CreatedAt string                     `json:"created_at"`
	Changes   []RecordHistoryFieldChange `json:"changes"`
}

type RecordHistoryFieldChange struct {
	FieldLabel string `json:"field_label"`
	FieldType  string `json:"field_type"`
	Before     any    `json:"before"`
	After      any    `json:"after"`
}

// HandleRecordHistory returns successful create, update, and delete activity
// for a record only after applying the same read and row-scope checks as GET.
func (h *Handler) HandleRecordHistory(c *gin.Context) {
	doctypeName := c.Param("doctype")
	docName := c.Param("name")
	registry := h.siteRegistry(c)
	dt := registry.Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Document not found"}})
		return
	}
	scope, forbidden := checkPerm(c, registry, dt.Name, "read")
	if forbidden {
		return
	}
	if _, err := h.siteTx(c).GetDocWithScope(dt, docName, scope); err != nil {
		// Match GET's not-found behavior so history does not reveal records
		// outside the caller's row scope.
		if errors.Is(err, orm.ErrNotFound) {
			c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Document not found"}})
		} else {
			internalError(c, "record history access check failed", err)
		}
		return
	}

	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	database := h.siteTx(c).DB
	query := `SELECT a.actor_user, a.command_name, a.created_at, h.changes_json
		FROM _kora_operation_audit a
		LEFT JOIN _kora_record_history h ON h.site = a.site AND h.operation_id = a.operation_id
			AND h.doctype = a.doctype AND h.doc_name = a.doc_name
		WHERE a.site = ? AND a.doctype = ? AND a.doc_name = ? AND a.status = 'completed'
		AND a.command_name IN ('record.create', 'record.update', 'record.delete')
		ORDER BY a.created_at DESC LIMIT ?`
	rows, err := database.Query(db.Rebind(h.siteDialect(c), query), c.GetString("site_name"), dt.Name, docName, limit)
	if err != nil {
		internalError(c, "record history query failed", err)
		return
	}
	defer rows.Close()
	entries := make([]RecordHistoryEntry, 0)
	visibleFields := make(map[string]doctype.Field, len(dt.Fields))
	for _, field := range dt.DataFields() {
		if !field.Hidden && field.Fieldtype != "Password" {
			visibleFields[field.Fieldname] = field
		}
	}
	for rows.Next() {
		var entry RecordHistoryEntry
		var command string
		var createdAt sql.NullString
		var changesJSON sql.NullString
		if err := rows.Scan(&entry.Actor, &command, &createdAt, &changesJSON); err != nil {
			internalError(c, "record history scan failed", err)
			return
		}
		switch strings.TrimSpace(command) {
		case "record.create":
			entry.Action = "created"
		case "record.update":
			entry.Action = "updated"
		case "record.delete":
			entry.Action = "deleted"
		}
		if createdAt.Valid {
			entry.CreatedAt = createdAt.String
		}
		entry.Changes = make([]RecordHistoryFieldChange, 0)
		if changesJSON.Valid {
			var stored []kernel.RecordFieldChange
			if err := json.Unmarshal([]byte(changesJSON.String), &stored); err != nil {
				internalError(c, "record history decode failed", err)
				return
			}
			for _, change := range stored {
				field, visible := visibleFields[change.Field]
				if !visible {
					continue
				}
				label := strings.TrimSpace(field.Label)
				if label == "" {
					label = field.Fieldname
				}
				entry.Changes = append(entry.Changes, RecordHistoryFieldChange{FieldLabel: label, FieldType: field.Fieldtype, Before: change.Before, After: change.After})
			}
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		internalError(c, "record history iteration failed", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: entries})
}
