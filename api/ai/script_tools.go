package ai

import (
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/script"
)

// executeScriptCreate creates a new script.
func executeScriptCreate(tx *orm.TxManager, args map[string]any, siteName, owner string) string {
	if tx.ScriptStore == nil {
		return "Error: script store is not available on this site."
	}

	name, _ := args["name"].(string)
	if name == "" {
		return "Error: 'name' is required. Use lowercase letters and underscores."
	}

	scriptType, _ := args["script_type"].(string)
	if scriptType == "" {
		return "Error: 'script_type' is required. Must be one of: doc_event, api_method, workflow_action, scheduled, computed, validate."
	}

	code, _ := args["script"].(string)
	if code == "" {
		return "Error: 'script' is required. Provide the JavaScript code to execute."
	}

	// Validate required fields per type.
	switch scriptType {
	case "doc_event":
		doctype, _ := args["doctype"].(string)
		if doctype == "" {
			return "Error: 'doctype' is required for doc_event scripts."
		}
		event, _ := args["event"].(string)
		if event == "" {
			return "Error: 'event' is required for doc_event scripts. Valid events: before_insert, after_insert, before_save, after_save, before_delete, after_delete, before_submit, after_submit, before_cancel, after_cancel, validate."
		}
	case "api_method":
		methodPath, _ := args["method_path"].(string)
		if methodPath == "" {
			return "Error: 'method_path' is required for api_method scripts (e.g., 'send_invoice')."
		}
	case "scheduled":
		schedule, _ := args["schedule"].(string)
		if schedule == "" {
			return "Error: 'schedule' is required for scheduled scripts (e.g., '0 9 * * *' for daily at 9am)."
		}
	case "workflow_action":
		workflowAction, _ := args["workflow_action"].(string)
		if workflowAction == "" {
			return "Error: 'workflow_action' is required for workflow_action scripts."
		}
	}

	// Check for duplicate name.
	existing, _ := tx.ScriptStore.LoadByName(siteName, name)
	if existing != nil {
		return fmt.Sprintf("Error: a script named %q already exists. Use a different name or update the existing one.", name)
	}

	// Validate the JavaScript syntax.
	if tx.ScriptRunner != nil {
		if err := tx.ScriptRunner.Validate(code); err != nil {
			return fmt.Sprintf("Error: script validation failed:\n%v\n\nFix the errors and try again.", err)
		}
	}

	priority := 10
	if p, ok := args["priority"].(float64); ok {
		priority = int(p)
	}
	timeoutMs := 5000
	if t, ok := args["timeout_ms"].(float64); ok {
		timeoutMs = int(t)
	}
	if owner == "" || owner == "mcp-agent" {
		owner = "ai-assistant"
	}

	rec := script.ScriptRecord{
		Name:           name,
		ScriptType:     script.Type(scriptType),
		Script:         code,
		DocType:        stringOr(args, "doctype"),
		Event:          script.Event(stringOr(args, "event")),
		MethodPath:     stringOr(args, "method_path"),
		WorkflowAction: stringOr(args, "workflow_action"),
		Schedule:       stringOr(args, "schedule"),
		Priority:       priority,
		TimeoutMs:      timeoutMs,
		IsActive:       true,
		RunAs:          stringOr(args, "run_as"),
		Site:           siteName,
	}

	if err := tx.ScriptStore.Insert(rec); err != nil {
		return fmt.Sprintf("Error creating script: %v", err)
	}

	return fmt.Sprintf("✓ Created script %q (type: %s). The script will run on the configured event/trigger. View it at /workspace/admin/scripts.", name, scriptType)
}

// executeScriptList lists all scripts for the current site.
func executeScriptList(tx *orm.TxManager, siteName string) string {
	if tx.ScriptStore == nil {
		return "Error: script store is not available on this site."
	}

	scripts, err := tx.ScriptStore.LoadAllForSite(siteName)
	if err != nil {
		return fmt.Sprintf("Error loading scripts: %v", err)
	}

	if len(scripts) == 0 {
		return "No scripts found. Create one to automate your business logic."
	}

	var lines []string
	for _, s := range scripts {
		active := "✓"
		if !s.IsActive {
			active = "✗"
		}
		detail := string(s.ScriptType)
		if s.DocType != "" {
			detail += " on " + s.DocType
		}
		if s.Event != "" {
			detail += " (" + string(s.Event) + ")"
		}
		if s.MethodPath != "" {
			detail += " at /api/method/" + s.MethodPath
		}
		lines = append(lines, fmt.Sprintf("%s %s — %s — %s", active, s.Name, detail, truncateScript(s.Script, 60)))
	}
	return strings.Join(lines, "\n")
}

// executeScriptValidate validates JavaScript syntax without saving.
func executeScriptValidate(tx *orm.TxManager, args map[string]any) string {
	code, _ := args["script"].(string)
	if code == "" {
		return "Error: 'script' is required."
	}

	if tx.ScriptRunner == nil {
		return "Error: script runner is not available on this site."
	}

	if err := tx.ScriptRunner.Validate(code); err != nil {
		return fmt.Sprintf("Validation failed:\n%v", err)
	}

	return "✓ Script syntax is valid."
}

// executeScriptGet returns a single script's details.
func executeScriptGet(tx *orm.TxManager, args map[string]any, siteName string) string {
	if tx.ScriptStore == nil {
		return "Error: script store is not available on this site."
	}

	name, _ := args["name"].(string)
	if name == "" {
		return "Error: 'name' is required."
	}

	rec, err := tx.ScriptStore.LoadByName(siteName, name)
	if err != nil || rec == nil {
		return fmt.Sprintf("Script %q not found.", name)
	}

	active := "active"
	if !rec.IsActive {
		active = "inactive"
	}

	return fmt.Sprintf("Script: %s\nType: %s\nStatus: %s\nDoctype: %s\nEvent: %s\nPriority: %d\nTimeout: %dms\n\n```javascript\n%s\n```",
		rec.Name, rec.ScriptType, active, rec.DocType, rec.Event, rec.Priority, rec.TimeoutMs, rec.Script)
}

// executeScriptExecutions returns execution logs for a script.
func executeScriptExecutions(tx *orm.TxManager, args map[string]any, siteName string) string {
	if tx.ScriptStore == nil {
		return "Error: script store is not available on this site."
	}

	name, _ := args["name"].(string)
	if name == "" {
		return "Error: 'name' is required."
	}

	logs, err := tx.ScriptStore.LoadExecutions(siteName, name, 10)
	if err != nil {
		return fmt.Sprintf("Error loading executions: %v", err)
	}

	if len(logs) == 0 {
		return fmt.Sprintf("No execution logs for script %q.", name)
	}

	var lines []string
	for _, l := range logs {
		status := l["status"]
		duration := l["duration_ms"]
		errMsg := ""
		if e, ok := l["error_message"].(string); ok && e != "" {
			errMsg = " — " + e
		}
		lines = append(lines, fmt.Sprintf("%v — %v (%vms)%s", l["created_at"], status, duration, errMsg))
	}
	return fmt.Sprintf("Last %d executions for %q:\n%s", len(logs), name, strings.Join(lines, "\n"))
}

func stringOr(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func truncateScript(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	// Take first line only.
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
