package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
)

const maxAIMutationBundleRecords = 10
const maxAIMutationChildRowsPerField = 100
const maxAIMutationChildRows = 250

var aiBundleKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func mutateRecordsToolDef(reg *doctype.Registry) map[string]any {
	description := "Create or update 2–10 related records as one atomic operation. Always find existing records first, show the user the complete proposed changes, and wait for approval before calling this tool. Creates may refer to other created records with a field value like $records.company.name. Updates that change workflow state must use transition_action; never write a workflow state field directly. Every update needs the revision returned by get. On creates, Table fields accept arrays of child-row objects using the configured child DocType's writable fields. Engine authorization and configured workflow rules apply to each record, and all writes commit or roll back together."
	if guide := aiBundleChildFieldGuide(reg); guide != "" {
		description += " Child-row fields: " + guide
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "mutate_records",
			"description": description,
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"records": map[string]any{
						"type": "array", "minItems": 2, "maxItems": maxAIMutationBundleRecords,
						"items": map[string]any{
							"type": "object", "additionalProperties": false,
							"properties": map[string]any{
								"key":               map[string]any{"type": "string", "description": "Unique reference key for this operation, used by $records.<key>.name"},
								"operation":         map[string]any{"type": "string", "enum": []string{"create", "update"}},
								"doctype":           map[string]any{"type": "string", "description": "Existing configured DocType name"},
								"name":              map[string]any{"type": "string", "description": "Existing document name for updates"},
								"expected_revision": map[string]any{"type": "integer", "minimum": 1, "description": "Revision returned by the get tool; required for updates"},
								"transition_action": map[string]any{"type": "string", "description": "Registered workflow transition action for an update, if needed"},
								"data":              map[string]any{"type": "object", "description": "Writable fields from this DocType's current tool contract"},
							},
							"required": []string{"key", "doctype", "data"},
						},
					},
				},
				"required": []string{"records"},
			},
		},
	}
}

func aiBundleChildFieldGuide(reg *doctype.Registry) string {
	if reg == nil {
		return ""
	}
	var entries []string
	for _, parent := range reg.All() {
		if parent.IsChildTable {
			continue
		}
		for _, field := range parent.DataFields() {
			if field.Fieldtype != "Table" {
				continue
			}
			child := aiBundleDocType(reg, field.Options)
			if child == nil || !child.IsChildTable {
				continue
			}
			var names []string
			for _, childField := range child.DataFields() {
				if childField.Fieldtype == "Table" || !aiReadableField(childField) || childField.ReadOnly || childField.Computed != "" {
					continue
				}
				name := childField.Fieldname
				if childField.Reqd {
					name += " (required)"
				}
				names = append(names, name)
			}
			if len(names) > 0 {
				entries = append(entries, parent.Name+"."+field.Fieldname+" → "+child.Name+" {"+strings.Join(names, ", ")+"}")
			}
		}
	}
	return strings.Join(entries, "; ")
}

func buildAIMutationBundle(reg *doctype.Registry, args map[string]any) (kernel.RecordMutationBundlePayload, error) {
	if reg == nil {
		return kernel.RecordMutationBundlePayload{}, fmt.Errorf("doctype registry unavailable")
	}
	records, ok := args["records"].([]any)
	if !ok || len(records) < 2 || len(records) > maxAIMutationBundleRecords {
		return kernel.RecordMutationBundlePayload{}, fmt.Errorf("records must contain 2 to %d operations", maxAIMutationBundleRecords)
	}
	payload := kernel.RecordMutationBundlePayload{Records: make([]kernel.RecordMutationBundleItem, 0, len(records))}
	childRowCount := 0
	for index, raw := range records {
		entry, ok := raw.(map[string]any)
		if !ok {
			return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %d must be an object", index+1)
		}
		key, _ := entry["key"].(string)
		doctypeName, _ := entry["doctype"].(string)
		operation, _ := entry["operation"].(string)
		name, _ := entry["name"].(string)
		action, _ := entry["transition_action"].(string)
		data, ok := entry["data"].(map[string]any)
		if !ok {
			return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q data must be an object", key)
		}
		if !aiBundleKeyPattern.MatchString(key) {
			return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record key %q must start with a letter and contain only letters, digits, underscores, or hyphens", key)
		}
		dt := aiBundleDocType(reg, doctypeName)
		if dt == nil || dt.IsChildTable {
			return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q references an unavailable DocType", key)
		}
		if operation == "" {
			operation = "create"
		}
		if operation != "create" && operation != "update" {
			return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q operation must be create or update", key)
		}
		if operation == "update" {
			if strings.TrimSpace(name) == "" {
				return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q update requires a document name", key)
			}
			_, ok := positiveBundleRevision(entry["expected_revision"])
			if !ok {
				return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q update requires its current expected_revision from get", key)
			}
		} else if name != "" || entry["expected_revision"] != nil || action != "" {
			return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q create cannot include name, expected_revision, or transition_action", key)
		}
		for fieldName := range data {
			field := dt.GetField(fieldName)
			if field == nil || !aiReadableField(*field) {
				return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q field %q is unknown or not writable", key, fieldName)
			}
			if field.Fieldtype == "Table" {
				if operation != "create" {
					return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q table field %q can only be supplied when creating a record", key, fieldName)
				}
				rows, ok := data[fieldName].([]any)
				if !ok || len(rows) > maxAIMutationChildRowsPerField || childRowCount+len(rows) > maxAIMutationChildRows {
					return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q table field %q must contain at most %d child rows and the operation at most %d total", key, fieldName, maxAIMutationChildRowsPerField, maxAIMutationChildRows)
				}
				childDT := aiBundleDocType(reg, field.Options)
				if childDT == nil || !childDT.IsChildTable {
					return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q table field %q has no configured child DocType", key, fieldName)
				}
				for rowIndex, rawRow := range rows {
					row, ok := rawRow.(map[string]any)
					if !ok {
						return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q table field %q row %d must be an object", key, fieldName, rowIndex+1)
					}
					for childFieldName := range row {
						childField := childDT.GetField(childFieldName)
						if childField == nil || !aiReadableField(*childField) || childField.Fieldtype == "Table" || childField.ReadOnly || childField.Computed != "" {
							return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q table field %q row %d field %q is unknown or not writable", key, fieldName, rowIndex+1, childFieldName)
						}
					}
				}
				childRowCount += len(rows)
				continue
			}
			if field.ReadOnly || field.Computed != "" {
				return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q field %q is unknown or not writable", key, fieldName)
			}
		}
		if operation == "update" && len(data) == 0 && action == "" {
			return kernel.RecordMutationBundlePayload{}, fmt.Errorf("record %q update needs writable fields or a transition_action", key)
		}
		dataJSON, err := json.Marshal(data)
		if err != nil {
			return kernel.RecordMutationBundlePayload{}, fmt.Errorf("encode record %q fields: %w", key, err)
		}
		item := kernel.RecordMutationBundleItem{Key: key, Operation: operation, Doctype: dt.Name, Name: strings.TrimSpace(name), TransitionAction: strings.TrimSpace(action), Data: dataJSON}
		if operation == "update" {
			item.ExpectedRevision, _ = positiveBundleRevision(entry["expected_revision"])
		}
		payload.Records = append(payload.Records, item)
	}
	payload.Doctype = payload.Records[0].Doctype
	return payload, nil
}

func aiBundleDocType(reg *doctype.Registry, name string) *doctype.DocType {
	if dt := reg.Get(name); dt != nil {
		return dt
	}
	for _, dt := range reg.All() {
		if strings.EqualFold(sanitizeName(dt.Name), sanitizeName(name)) {
			return dt
		}
	}
	return nil
}

func positiveBundleRevision(value any) (uint64, bool) {
	switch n := value.(type) {
	case float64:
		if n < 1 || n > math.MaxInt64 || math.Trunc(n) != n {
			return 0, false
		}
		return uint64(n), true
	case json.Number:
		parsed, err := n.Int64()
		return uint64(parsed), err == nil && parsed > 0
	case int:
		return uint64(n), n > 0
	case int64:
		return uint64(n), n > 0
	case uint64:
		return n, n > 0
	default:
		return 0, false
	}
}

func executeAIMutationBundle(tx *orm.TxManager, reg *doctype.Registry, args map[string]any, owner, site, runID, stepID, idempotencyKey string, execution policy.Execution) string {
	if tx == nil || tx.DB == nil || !execution.Valid() {
		return "Permission denied: verified execution and tenant database are required"
	}
	if strings.TrimSpace(runID) == "" {
		return "Error: atomic multi-record writes require a durable AI run so user approval can be recorded."
	}
	payload, err := buildAIMutationBundle(reg, args)
	if err != nil {
		return "Error preparing related record changes: " + err.Error()
	}
	approved, err := HasGrantedApproval(tx.Context, tx.DB, site, runID, "mutate_records", args)
	if err != nil {
		return "Error checking approval for related record changes; no changes were made."
	}
	if !approved {
		actor := execution.Actor()
		principal := actor.SubjectUserID
		if principal == "" {
			principal = actor.PrincipalID
		}
		if err := EnsureApprovalPending(tx.Context, tx.DB, site, runID, principal, "agent", "mutate_records", 0, args); err != nil {
			return "Error recording approval for related record changes; no changes were made."
		}
		if stepID != "" {
			if err := MarkRunPendingApproval(tx.Context, tx.DB, runID, stepID, "mutate_records_pending_approval"); err != nil {
				return "Error recording approval state for related record changes; no changes were made."
			}
		}
		return "Approval required for mutate_records. Review the full related-record change and approve it before execution; no records were changed."
	}
	actor := execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	role := ""
	if len(actor.Roles) > 0 {
		role = actor.Roles[0]
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "Error encoding related record changes: " + err.Error()
	}
	tx.Execution = execution
	engine := kernel.New(tx.Dialect, tx.Outbox)
	engine.TxManager = tx
	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	result, cerr := engine.Execute(ctx, tx.DB, reg, kernel.Operation{
		Context: kernel.OperationContext{Site: site, TenantID: execution.Tenant(), Execution: execution, BusinessScope: tx.BusinessScope, Actor: actor, User: user, UserRole: role, Roles: actor.Roles, Source: kernel.SourceAI, IdempotencyKey: idempotencyKey},
		Command: kernel.CommandRecordMutateBundle, Payload: raw,
	})
	if cerr != nil {
		return "Error applying related record changes: " + cerr.Error()
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(result.Data, &operation); err != nil {
		return "Error reading related record result: " + err.Error()
	}
	changes := []string{fmt.Sprintf("%s %q", operation.Doctype, operation.Name)}
	for _, related := range operation.Related {
		verb := "updated"
		if related.Created {
			verb = "created"
		}
		changes = append(changes, fmt.Sprintf("%s %s %q", verb, related.Doctype, related.Name))
	}
	return "Applied the approved atomic operation: " + strings.Join(changes, "; ") + "."
}
