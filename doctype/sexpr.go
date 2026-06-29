package doctype

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// ToSExpr -- ConfigSnapshot -> canonical s-expression string
// ---------------------------------------------------------------------------

// ToSExpr converts a ConfigSnapshot to a canonical s-expression string.
// Canonical form: sections in fixed order, entities sorted by name,
// keyword args alphabetically.
func ToSExpr(snapshot *ConfigSnapshot) string {
	if snapshot == nil {
		return "(config)"
	}
	var b strings.Builder
	b.WriteString("(config")

	if snapshot.MinKoraVersion != "" {
		b.WriteString("\n  :min-kora-version ")
		b.WriteString(quoteSExprString(snapshot.MinKoraVersion))
	}

	writeDoctypesSection(&b, snapshot.DocTypes)
	writeRolesSection(&b, snapshot.Roles)
	writePermissionsSection(&b, snapshot.Permissions)
	writeWorkflowsSection(&b, snapshot.Workflows)
	writeAnalyticsMetricsSection(&b, snapshot.AnalyticsMetrics)
	writeScriptsSection(&b, snapshot.Scripts)

	b.WriteString(")")
	return b.String()
}

// ---------------------------------------------------------------------------
// Section writers
// ---------------------------------------------------------------------------

func writeDoctypesSection(b *strings.Builder, doctypes []*DocType) {
	if len(doctypes) == 0 {
		b.WriteString("\n  (doctypes)")
		return
	}

	// Sort by name
	sorted := sortedDT(doctypes)

	b.WriteString("\n  (doctypes")
	for _, dt := range sorted {
		writeSExprDocType(b, dt, "    ")
	}
	b.WriteString(")")
}

func writeRolesSection(b *strings.Builder, roles []*Role) {
	if len(roles) == 0 {
		b.WriteString("\n  (roles)")
		return
	}
	sorted := sortedRoles(roles)
	b.WriteString("\n  (roles")
	for _, r := range sorted {
		writeSExprRole(b, r, "    ")
	}
	b.WriteString(")")
}

func writePermissionsSection(b *strings.Builder, perms []*Permission) {
	if len(perms) == 0 {
		b.WriteString("\n  (permissions)")
		return
	}
	sorted := sortedPerms(perms)
	b.WriteString("\n  (permissions")
	for _, p := range sorted {
		writeSExprPermission(b, p, "    ")
	}
	b.WriteString(")")
}

func writeWorkflowsSection(b *strings.Builder, workflows []*Workflow) {
	if len(workflows) == 0 {
		b.WriteString("\n  (workflows)")
		return
	}
	sorted := sortedWorkflows(workflows)
	b.WriteString("\n  (workflows")
	for _, w := range sorted {
		writeSExprWorkflow(b, w, "    ")
	}
	b.WriteString(")")
}

func writeAnalyticsMetricsSection(b *strings.Builder, metrics []*AnalyticsMetricConfig) {
	if len(metrics) == 0 {
		b.WriteString("\n  (analytics-metrics)")
		return
	}
	sorted := sortedMetrics(metrics)
	b.WriteString("\n  (analytics-metrics")
	for _, m := range sorted {
		writeSExprAnalyticsMetric(b, m, "    ")
	}
	b.WriteString(")")
}

func writeScriptsSection(b *strings.Builder, scripts []*ScriptSnapshot) {
	if len(scripts) == 0 {
		b.WriteString("\n  (scripts)")
		return
	}
	sorted := sortedScriptSnapshots(scripts)
	b.WriteString("\n  (scripts")
	for _, s := range sorted {
		writeSExprScriptSnapshot(b, s, "    ")
	}
	b.WriteString(")")
}

// ---------------------------------------------------------------------------
// Entity writers
// ---------------------------------------------------------------------------

type sExprProp struct {
	key string
	val string // pre-formatted s-expression value
}

// writeSortedProps writes alphabetically sorted keyword args.
func writeSortedProps(b *strings.Builder, indent string, props []sExprProp) {
	sort.Slice(props, func(i, j int) bool { return props[i].key < props[j].key })
	for _, p := range props {
		b.WriteString(indent)
		b.WriteString(":")
		b.WriteString(p.key)
		b.WriteString(" ")
		b.WriteString(p.val)
	}
}

func writeSExprDocType(b *strings.Builder, dt *DocType, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(doctype ")
	b.WriteString(symbolName(dt.Name))

	// Collect keyword props
	var props []sExprProp
	if dt.Module != "" {
		props = append(props, sExprProp{"module", quoteSExprString(dt.Module)})
	}
	if dt.IsSubmittable {
		props = append(props, sExprProp{"submittable", "true"})
	}
	if dt.IsChildTable {
		props = append(props, sExprProp{"child-table", "true"})
	}
	if dt.IsSingle {
		props = append(props, sExprProp{"single", "true"})
	}
	if dt.TrackChanges {
		props = append(props, sExprProp{"track-changes", "true"})
	}
	if dt.TitleField != "" {
		props = append(props, sExprProp{"title-field", quoteSExprString(dt.TitleField)})
	}
	if dt.SearchFields != "" {
		props = append(props, sExprProp{"search-fields", quoteSExprString(dt.SearchFields)})
	}
	if dt.SortField != "" {
		props = append(props, sExprProp{"sort-field", quoteSExprString(dt.SortField)})
	}
	if dt.SortOrder != "" {
		props = append(props, sExprProp{"sort-order", quoteSExprString(dt.SortOrder)})
	}
	if dt.Description != "" {
		props = append(props, sExprProp{"description", quoteSExprString(dt.Description)})
	}

	// Fields sorted by name
	subIndent := indent + "  "
	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, subIndent, props)
	}

	sortedFields := sortedFieldsByName(dt.Fields)
	for _, f := range sortedFields {
		writeSExprField(b, f, subIndent)
	}

	// DocConstraints sorted by type
	if len(dt.DocConstraints) > 0 {
		sortedDC := sortedDocConstraints(dt.DocConstraints)
		for _, dc := range sortedDC {
			writeSExprDocConstraint(b, dc, subIndent)
		}
	}

	b.WriteString(")")
}

func writeSExprField(b *strings.Builder, f Field, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(field ")
	b.WriteString(symbolName(f.Fieldname))

	var props []sExprProp
	props = append(props, sExprProp{"type", symbolName(f.Fieldtype)})

	if f.Label != "" && f.Label != f.Fieldname {
		props = append(props, sExprProp{"label", quoteSExprString(f.Label)})
	}
	if f.Options != "" {
		if f.Fieldtype == "Link" {
			props = append(props, sExprProp{"to", symbolName(f.Options)})
		} else {
			props = append(props, sExprProp{"options", quoteSExprString(f.Options)})
		}
	}
	if f.Reqd {
		props = append(props, sExprProp{"required", "true"})
	}
	if f.Unique {
		props = append(props, sExprProp{"unique", "true"})
	}
	if f.Default != "" {
		props = append(props, sExprProp{"default", quoteSExprString(f.Default)})
	}
	if f.Hidden {
		props = append(props, sExprProp{"hidden", "true"})
	}
	if f.ReadOnly {
		props = append(props, sExprProp{"read-only", "true"})
	}
	if f.Bold {
		props = append(props, sExprProp{"bold", "true"})
	}
	if f.InListView {
		props = append(props, sExprProp{"in-list-view", "true"})
	}
	if f.InStandardFilter {
		props = append(props, sExprProp{"in-standard-filter", "true"})
	}
	if f.SearchIndex {
		props = append(props, sExprProp{"search-index", "true"})
	}
	if f.Description != "" {
		props = append(props, sExprProp{"description", quoteSExprString(f.Description)})
	}
	if f.DependsOn != "" {
		props = append(props, sExprProp{"depends-on", quoteSExprString(f.DependsOn)})
	}
	if f.MandatoryDependsOn != "" {
		props = append(props, sExprProp{"mandatory-depends-on", quoteSExprString(f.MandatoryDependsOn)})
	}
	if f.RenamedFrom != "" {
		props = append(props, sExprProp{"renamed-from", quoteSExprString(f.RenamedFrom)})
	}
	if f.LinkedField != "" {
		props = append(props, sExprProp{"linked-field", quoteSExprString(f.LinkedField)})
	}
	if f.Computed != "" {
		props = append(props, sExprProp{"computed", quoteSExprString(f.Computed)})
	}
	if f.DependencyScope != "" {
		props = append(props, sExprProp{"dependency-scope", quoteSExprString(f.DependencyScope)})
	}

	subIndent := indent + "  "
	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, subIndent, props)
	}

	// Constraints sorted by type
	if len(f.Constraints) > 0 {
		sortedConstraints := sortedConstraints(f.Constraints)
		for _, c := range sortedConstraints {
			writeSExprConstraint(b, c, subIndent)
		}
	}

	b.WriteString(")")
}

func writeSExprConstraint(b *strings.Builder, c Constraint, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(constraint ")
	b.WriteString(symbolName(c.Type))

	var props []sExprProp
	if c.Value != nil {
		props = append(props, sExprProp{"value", formatAnyValue(c.Value)})
	}
	if len(c.Values) > 0 {
		props = append(props, sExprProp{"values", quoteSExprString(strings.Join(c.Values, " "))})
	}
	if c.Pattern != "" {
		props = append(props, sExprProp{"pattern", quoteSExprString(c.Pattern)})
	}
	if c.Message != "" {
		props = append(props, sExprProp{"message", quoteSExprString(c.Message)})
	}
	if c.Condition != "" {
		props = append(props, sExprProp{"condition", quoteSExprString(c.Condition)})
	}
	if c.Scope != "" {
		props = append(props, sExprProp{"scope", quoteSExprString(c.Scope)})
	}

	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, indent+"  ", props)
	}
	b.WriteString(")")
}

func writeSExprDocConstraint(b *strings.Builder, dc DocConstraint, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(doc-constraint ")
	b.WriteString(symbolName(dc.Type))

	var props []sExprProp
	if dc.Description != "" {
		props = append(props, sExprProp{"description", quoteSExprString(dc.Description)})
	}
	if dc.Condition != "" {
		props = append(props, sExprProp{"condition", quoteSExprString(dc.Condition)})
	}
	if len(dc.RequireFields) > 0 {
		props = append(props, sExprProp{"require-fields", quoteSExprString(strings.Join(dc.RequireFields, " "))})
	}
	if dc.Field != "" {
		props = append(props, sExprProp{"field", quoteSExprString(dc.Field)})
	}
	if len(dc.GroupBy) > 0 {
		props = append(props, sExprProp{"group-by", quoteSExprString(strings.Join(dc.GroupBy, " "))})
	}
	if dc.Max != 0 {
		props = append(props, sExprProp{"max", formatFloat(dc.Max)})
	}
	if dc.Message != "" {
		props = append(props, sExprProp{"message", quoteSExprString(dc.Message)})
	}
	if dc.LHS != "" {
		props = append(props, sExprProp{"lhs", quoteSExprString(dc.LHS)})
	}
	if dc.Operator != "" {
		props = append(props, sExprProp{"operator", quoteSExprString(dc.Operator)})
	}
	if dc.RHS != "" {
		props = append(props, sExprProp{"rhs", quoteSExprString(dc.RHS)})
	}
	if len(dc.Fields) > 0 {
		props = append(props, sExprProp{"fields", quoteSExprString(strings.Join(dc.Fields, " "))})
	}
	if dc.StatusField != "" {
		props = append(props, sExprProp{"status-field", quoteSExprString(dc.StatusField)})
	}
	if len(dc.StatusValues) > 0 {
		props = append(props, sExprProp{"status-values", quoteSExprString(strings.Join(dc.StatusValues, " "))})
	}
	if len(dc.ImmutableFields) > 0 {
		props = append(props, sExprProp{"immutable-fields", quoteSExprString(strings.Join(dc.ImmutableFields, " "))})
	}
	if len(dc.Constraints) > 0 {
		var cs []string
		for _, sc := range dc.Constraints {
			cs = append(cs, sc.Type)
		}
		props = append(props, sExprProp{"constraints", quoteSExprString(strings.Join(cs, " "))})
	}

	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, indent+"  ", props)
	}
	b.WriteString(")")
}

func writeSExprRole(b *strings.Builder, r *Role, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(role ")
	b.WriteString(symbolName(r.Name))

	var props []sExprProp
	if r.WorkspaceAccess {
		props = append(props, sExprProp{"workspace", "true"})
	}
	if r.Description != "" {
		props = append(props, sExprProp{"description", quoteSExprString(r.Description)})
	}

	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, indent+"  ", props)
	}
	b.WriteString(")")
}

func writeSExprPermission(b *strings.Builder, p *Permission, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(perm ")
	b.WriteString(symbolName(p.Doctype))
	b.WriteString(" ")
	b.WriteString(symbolName(p.Role))

	var props []sExprProp
	if p.Read {
		props = append(props, sExprProp{"read", "true"})
	}
	if p.Write {
		props = append(props, sExprProp{"write", "true"})
	}
	if p.Create {
		props = append(props, sExprProp{"create", "true"})
	}
	if p.Delete {
		props = append(props, sExprProp{"delete", "true"})
	}
	if p.Submit {
		props = append(props, sExprProp{"submit", "true"})
	}
	if p.Cancel {
		props = append(props, sExprProp{"cancel", "true"})
	}
	if p.Amend {
		props = append(props, sExprProp{"amend", "true"})
	}
	if p.Export {
		props = append(props, sExprProp{"export", "true"})
	}
	if p.Import {
		props = append(props, sExprProp{"import", "true"})
	}
	if p.Report {
		props = append(props, sExprProp{"report", "true"})
	}
	if p.IfOwner {
		props = append(props, sExprProp{"if-owner", "true"})
	}

	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, indent+"  ", props)
	}
	b.WriteString(")")
}

func writeSExprWorkflow(b *strings.Builder, w *Workflow, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(workflow ")
	b.WriteString(symbolName(w.Name))

	var props []sExprProp
	if w.DocumentType != "" {
		props = append(props, sExprProp{"on", symbolName(w.DocumentType)})
	}
	props = append(props, sExprProp{"active", formatBool(w.IsActive)})
	if w.WorkflowStateField != "" && w.WorkflowStateField != "status" {
		props = append(props, sExprProp{"state-field", quoteSExprString(w.WorkflowStateField)})
	}

	subIndent := indent + "  "
	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, subIndent, props)
	}

	// States sorted by DocStatus
	sortedStates := sortedWorkflowStates(w.States)
	for _, s := range sortedStates {
		writeSExprWorkflowState(b, s, subIndent)
	}

	// Transitions sorted by action name
	sortedTrans := sortedWorkflowTransitions(w.Transitions)
	for _, t := range sortedTrans {
		writeSExprWorkflowTransition(b, t, subIndent)
	}

	// Notifications sorted by event+toState
	sortedNotifs := sortedWorkflowNotifications(w.Notifications)
	for _, n := range sortedNotifs {
		writeSExprWorkflowNotification(b, n, subIndent)
	}

	b.WriteString(")")
}

func writeSExprWorkflowState(b *strings.Builder, s WorkflowState, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(state ")
	b.WriteString(symbolName(s.State))

	var props []sExprProp
	props = append(props, sExprProp{"doc-status", strconv.Itoa(s.DocStatus)})
	if s.AllowEdit != "" {
		props = append(props, sExprProp{"allow-edit", quoteSExprString(s.AllowEdit)})
	}
	if s.Style != "" && s.Style != "default" {
		props = append(props, sExprProp{"style", quoteSExprString(s.Style)})
	}

	b.WriteString("\n")
	writeSortedProps(b, indent+"  ", props)
	b.WriteString(")")
}

func writeSExprWorkflowTransition(b *strings.Builder, t WorkflowTransition, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(action ")
	b.WriteString(symbolName(t.Action))

	var props []sExprProp
	if t.From != "" {
		props = append(props, sExprProp{"from", symbolName(t.From)})
	}
	if t.To != "" {
		props = append(props, sExprProp{"to", symbolName(t.To)})
	}
	if t.Allowed != "" {
		props = append(props, sExprProp{"allowed", quoteSExprString(t.Allowed)})
	}
	if t.Condition != "" {
		props = append(props, sExprProp{"condition", quoteSExprString(t.Condition)})
	}
	if len(t.RequireFields) > 0 {
		props = append(props, sExprProp{"require-fields", quoteSExprString(strings.Join(t.RequireFields, " "))})
	}

	subIndent := indent + "  "
	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, subIndent, props)
	}

	// Workflow actions on transition, on success, on failure
	for _, a := range t.OnTransition {
		writeSExprWorkflowAction(b, a, "on-transition", subIndent)
	}
	for _, a := range t.OnSuccess {
		writeSExprWorkflowAction(b, a, "on-success", subIndent)
	}
	for _, a := range t.OnFailure {
		writeSExprWorkflowAction(b, a, "on-failure", subIndent)
	}

	b.WriteString(")")
}

func writeSExprWorkflowAction(b *strings.Builder, a WorkflowAction, label, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(")
	b.WriteString(label)

	var props []sExprProp
	if a.Type != "" {
		props = append(props, sExprProp{"type", quoteSExprString(a.Type)})
	}
	if a.Script != "" {
		props = append(props, sExprProp{"script", quoteSExprString(a.Script)})
	}
	if a.WebhookURL != "" {
		props = append(props, sExprProp{"webhook-url", quoteSExprString(a.WebhookURL)})
	}
	if a.Condition != "" {
		props = append(props, sExprProp{"condition", quoteSExprString(a.Condition)})
	}
	if a.Async {
		props = append(props, sExprProp{"async", "true"})
	}

	b.WriteString("\n")
	writeSortedProps(b, indent+"  ", props)
	b.WriteString(")")
}

func writeSExprWorkflowNotification(b *strings.Builder, n WorkflowNotification, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(notification")

	var props []sExprProp
	if n.Event != "" {
		props = append(props, sExprProp{"event", quoteSExprString(n.Event)})
	}
	if n.ToState != "" {
		props = append(props, sExprProp{"to-state", quoteSExprString(n.ToState)})
	}
	if len(n.Recipients) > 0 {
		props = append(props, sExprProp{"recipients", formatRecipients(n.Recipients)})
	}
	if n.Subject != "" {
		props = append(props, sExprProp{"subject", quoteSExprString(n.Subject)})
	}
	if n.Message != "" {
		props = append(props, sExprProp{"message", quoteSExprString(n.Message)})
	}

	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, indent+"  ", props)
	}
	b.WriteString(")")
}

func formatRecipients(recipients []map[string]string) string {
	if len(recipients) == 0 {
		return "()"
	}
	var parts []string
	for _, m := range recipients {
		var kvs []string
		for k, v := range m {
			kvs = append(kvs, "("+quoteSExprString(k)+" "+quoteSExprString(v)+")")
		}
		parts = append(parts, strings.Join(kvs, " "))
	}
	return "(" + strings.Join(parts, " ") + ")"
}

func writeSExprAnalyticsMetric(b *strings.Builder, m *AnalyticsMetricConfig, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(metric ")
	b.WriteString(symbolName(m.Name))

	var props []sExprProp
	if m.Label != "" {
		props = append(props, sExprProp{"label", quoteSExprString(m.Label)})
	}
	if m.Type != "" {
		props = append(props, sExprProp{"type", symbolName(m.Type)})
	}
	if m.DocType != "" {
		props = append(props, sExprProp{"doctype", symbolName(m.DocType)})
	}
	if m.FieldName != "" {
		props = append(props, sExprProp{"field", quoteSExprString(m.FieldName)})
	}
	if m.LinkField != "" {
		props = append(props, sExprProp{"link-field", quoteSExprString(m.LinkField)})
	}
	if m.GroupByField != "" {
		props = append(props, sExprProp{"group-by", quoteSExprString(m.GroupByField)})
	}
	if m.AutoGenerated {
		props = append(props, sExprProp{"auto-generated", "true"})
	}

	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, indent+"  ", props)
	}
	b.WriteString(")")
}

func writeSExprScriptSnapshot(b *strings.Builder, s *ScriptSnapshot, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(script-ref ")
	b.WriteString(symbolName(s.Name))

	var props []sExprProp
	if s.ScriptType != "" {
		props = append(props, sExprProp{"script-type", symbolName(s.ScriptType)})
	}
	if s.DocType != "" {
		props = append(props, sExprProp{"doctype", symbolName(s.DocType)})
	}
	if s.Event != "" {
		props = append(props, sExprProp{"event", quoteSExprString(s.Event)})
	}
	if s.MethodPath != "" {
		props = append(props, sExprProp{"method", quoteSExprString(s.MethodPath)})
	}
	if s.WorkflowAction != "" {
		props = append(props, sExprProp{"action", quoteSExprString(s.WorkflowAction)})
	}
	if s.Schedule != "" {
		props = append(props, sExprProp{"schedule", quoteSExprString(s.Schedule)})
	}
	if s.Priority != 0 {
		props = append(props, sExprProp{"priority", strconv.Itoa(s.Priority)})
	}
	if s.IsActive {
		props = append(props, sExprProp{"active", "true"})
	}
	if s.RunAs != "" {
		props = append(props, sExprProp{"run-as", quoteSExprString(s.RunAs)})
	}
	if s.TimeoutMs != 0 {
		props = append(props, sExprProp{"timeout", strconv.Itoa(s.TimeoutMs)})
	}
	if s.ScriptHash != "" {
		props = append(props, sExprProp{"hash", quoteSExprString(s.ScriptHash)})
	}

	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, indent+"  ", props)
	}
	b.WriteString(")")
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// quoteSExprString wraps s in double quotes, escaping special characters.
func quoteSExprString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '\\':
			b.WriteString("\\\\")
		case '"':
			b.WriteString("\\\"")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '\r':
			b.WriteString("\\r")
		default:
			if ch < 0x20 {
				b.WriteString(fmt.Sprintf("\\x%02x", ch))
			} else {
				b.WriteByte(ch)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// symbolName returns s as a bare s-expression symbol, handling edge cases.
// If s contains characters that aren't valid in a bare symbol, it's quoted.
func symbolName(s string) string {
	if s == "" {
		return ""
	}
	// Symbols can contain letters, digits, hyphens, underscores.
	// If the name has other characters, quote it.
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !isIdentChar(ch) {
			return quoteSExprString(s)
		}
	}
	return s
}

func isIdentChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' ||
		ch == '#' || ch == '+' || ch == '/' || ch == '@'
}

func formatAnyValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return quoteSExprString(val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return formatFloat(val)
	default:
		return quoteSExprString(fmt.Sprintf("%v", v))
	}
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) && !math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
func formatBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ---------------------------------------------------------------------------
// Sorting helpers
// ---------------------------------------------------------------------------

func sortedDT(dts []*DocType) []*DocType {
	out := make([]*DocType, len(dts))
	copy(out, dts)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedFieldsByName(fields []Field) []Field {
	out := make([]Field, len(fields))
	copy(out, fields)
	sort.Slice(out, func(i, j int) bool { return out[i].Fieldname < out[j].Fieldname })
	return out
}

func sortedConstraints(cs []Constraint) []Constraint {
	out := make([]Constraint, len(cs))
	copy(out, cs)
	sort.Slice(out, func(i, j int) bool { return cs[i].Type < cs[j].Type })
	return out
}

func sortedDocConstraints(cs []DocConstraint) []DocConstraint {
	out := make([]DocConstraint, len(cs))
	copy(out, cs)
	sort.Slice(out, func(i, j int) bool { return cs[i].Type < cs[j].Type })
	return out
}

func sortedRoles(roles []*Role) []*Role {
	out := make([]*Role, len(roles))
	copy(out, roles)
	sort.Slice(out, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	return out
}

func sortedPerms(perms []*Permission) []*Permission {
	out := make([]*Permission, len(perms))
	copy(out, perms)
	sort.Slice(out, func(i, j int) bool {
		if perms[i].Doctype != perms[j].Doctype {
			return perms[i].Doctype < perms[j].Doctype
		}
		return perms[i].Role < perms[j].Role
	})
	return out
}

func sortedWorkflows(ws []*Workflow) []*Workflow {
	out := make([]*Workflow, len(ws))
	copy(out, ws)
	sort.Slice(out, func(i, j int) bool { return ws[i].Name < ws[j].Name })
	return out
}

func sortedWorkflowStates(states []WorkflowState) []WorkflowState {
	out := make([]WorkflowState, len(states))
	copy(out, states)
	sort.Slice(out, func(i, j int) bool { return states[i].DocStatus < states[j].DocStatus })
	return out
}

func sortedWorkflowTransitions(trans []WorkflowTransition) []WorkflowTransition {
	out := make([]WorkflowTransition, len(trans))
	copy(out, trans)
	sort.Slice(out, func(i, j int) bool {
		if trans[i].From != trans[j].From {
			return trans[i].From < trans[j].From
		}
		return trans[i].To < trans[j].To
	})
	return out
}

func sortedWorkflowNotifications(ns []WorkflowNotification) []WorkflowNotification {
	out := make([]WorkflowNotification, len(ns))
	copy(out, ns)
	sort.Slice(out, func(i, j int) bool {
		if ns[i].Event != ns[j].Event {
			return ns[i].Event < ns[j].Event
		}
		return ns[i].ToState < ns[j].ToState
	})
	return out
}

func sortedMetrics(ms []*AnalyticsMetricConfig) []*AnalyticsMetricConfig {
	out := make([]*AnalyticsMetricConfig, len(ms))
	copy(out, ms)
	sort.Slice(out, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	return out
}

func sortedScriptSnapshots(ss []*ScriptSnapshot) []*ScriptSnapshot {
	out := make([]*ScriptSnapshot, len(ss))
	copy(out, ss)
	sort.Slice(out, func(i, j int) bool { return ss[i].Name < ss[j].Name })
	return out
}

// ---------------------------------------------------------------------------
// s-expression tokenizer
// ---------------------------------------------------------------------------

type sTokenType int

const (
	sTokOpen sTokenType = iota
	sTokClose
	sTokKeyword
	sTokString
	sTokSymbol
	sTokBool
	sTokInt
	sTokFloat
	sTokEOF
)

type sToken struct {
	typ  sTokenType
	text string
}

type sLexer struct {
	input string
	pos   int
}

func newSLexer(input string) *sLexer {
	return &sLexer{input: input, pos: 0}
}

func (l *sLexer) skipWhitespace() {
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			l.pos++
		} else {
			break
		}
	}
}

func (l *sLexer) nextToken() (sToken, error) {
	l.skipWhitespace()
	if l.pos >= len(l.input) {
		return sToken{typ: sTokEOF}, nil
	}

	ch := l.input[l.pos]
	switch {
	case ch == '(':
		l.pos++
		return sToken{typ: sTokOpen, text: "("}, nil
	case ch == ')':
		l.pos++
		return sToken{typ: sTokClose, text: ")"}, nil
	case ch == '"':
		return l.readString()
	case ch == ':':
		return l.readKeyword()
	case ch == ';':
		// Comment — skip until end of line
		for l.pos < len(l.input) && l.input[l.pos] != '\n' {
			l.pos++
		}
		return l.nextToken()
	default:
		return l.readAtom()
	}
}

func (l *sLexer) readString() (sToken, error) {
	start := l.pos
	l.pos++ // skip opening "
	var b strings.Builder
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == '"' {
			l.pos++
			return sToken{typ: sTokString, text: b.String()}, nil
		}
		if ch == '\\' && l.pos+1 < len(l.input) {
			l.pos++
			switch l.input[l.pos] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case 'x':
				if l.pos+2 < len(l.input) {
					hex := l.input[l.pos+1 : l.pos+3]
					if val, err := strconv.ParseUint(hex, 16, 8); err == nil {
						b.WriteByte(byte(val))
						l.pos += 2
					} else {
						b.WriteByte('x')
					}
				} else {
					b.WriteByte('x')
				}
			default:
				b.WriteByte(l.input[l.pos])
			}
			l.pos++
		} else {
			b.WriteByte(ch)
			l.pos++
		}
	}
	return sToken{}, fmt.Errorf("unterminated string at position %d", start)
}

func (l *sLexer) readKeyword() (sToken, error) {
	l.pos++ // skip ':'
	start := l.pos
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '(' || ch == ')' {
			break
		}
		l.pos++
	}
	return sToken{typ: sTokKeyword, text: l.input[start:l.pos]}, nil
}

func (l *sLexer) readAtom() (sToken, error) {
	start := l.pos
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '(' || ch == ')' {
			break
		}
		l.pos++
	}
	word := l.input[start:l.pos]
	if word == "true" || word == "false" {
		return sToken{typ: sTokBool, text: word}, nil
	}
	if isNumber(word) {
		if strings.Contains(word, ".") {
			return sToken{typ: sTokFloat, text: word}, nil
		}
		return sToken{typ: sTokInt, text: word}, nil
	}
	return sToken{typ: sTokSymbol, text: word}, nil
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[i] == '-' || s[i] == '+' {
		i++
	}
	if i >= len(s) {
		return false
	}
	hasDigit := false
	hasDot := false
	for ; i < len(s); i++ {
		ch := s[i]
		if ch >= '0' && ch <= '9' {
			hasDigit = true
		} else if ch == '.' && !hasDot {
			hasDot = true
		} else {
			return false
		}
	}
	return hasDigit
}

// ---------------------------------------------------------------------------
// s-expression parser
// ---------------------------------------------------------------------------

// sNode is a parsed s-expression node.
type sNode struct {
	IsList   bool     // true = list with Children; false = atom
	Children []*sNode // for lists
	Atom     string   // for atoms: the text value
	AtomType sTokenType // for atoms: sTokSymbol, sTokString, sTokBool, sTokInt, sTokFloat, sTokKeyword
}

func parseSExpr(input string) (*sNode, error) {
	lexer := newSLexer(input)
	tok, err := lexer.nextToken()
	if err != nil {
		return nil, err
	}
	node, err := parseOne(lexer, tok)
	if err != nil {
		return nil, err
	}
	// Check for trailing tokens
	remaining, err := lexer.nextToken()
	if err != nil {
		return nil, err
	}
	if remaining.typ != sTokEOF {
		return nil, fmt.Errorf("unexpected trailing content after s-expression")
	}
	return node, nil
}

func parseOne(l *sLexer, tok sToken) (*sNode, error) {
	switch tok.typ {
	case sTokOpen:
		return parseList(l)
	default:
		return &sNode{
			IsList:   false,
			Atom:     tok.text,
			AtomType: tok.typ,
		}, nil
	}
}

func parseList(l *sLexer) (*sNode, error) {
	var children []*sNode
	for {
		tok, err := l.nextToken()
		if err != nil {
			return nil, err
		}
		switch tok.typ {
		case sTokClose:
			return &sNode{IsList: true, Children: children}, nil
		case sTokEOF:
			return nil, fmt.Errorf("unexpected end of input in list")
		default:
			child, err := parseOne(l, tok)
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		}
	}
}

// ---------------------------------------------------------------------------
// FromSExpr -- s-expression string -> ConfigSnapshot
// ---------------------------------------------------------------------------

// FromSExpr parses a canonical s-expression back into a ConfigSnapshot.
func FromSExpr(sexpr string) (*ConfigSnapshot, error) {
	node, err := parseSExpr(sexpr)
	if err != nil {
		return nil, fmt.Errorf("parsing s-expression: %w", err)
	}
	if !node.IsList || len(node.Children) == 0 {
		return nil, fmt.Errorf("expected (config ...)")
	}
	head := node.Children[0]
	if head.IsList || head.Atom != "config" {
		return nil, fmt.Errorf("expected (config ...), got %s", head.Atom)
	}
	return buildSnapshot(node.Children[1:])
}

func buildSnapshot(children []*sNode) (*ConfigSnapshot, error) {
	snapshot := &ConfigSnapshot{}
	for i := 0; i < len(children); i++ {
		child := children[i]
		if child.IsList {
			// Section list
			if len(child.Children) == 0 {
				continue
			}
			sectionHead := nodeStr(child.Children[0])
			switch sectionHead {
			case "doctypes":
				dts, err := parseDocTypes(child.Children[1:])
				if err != nil {
					return nil, err
				}
				snapshot.DocTypes = dts
			case "roles":
				roles, err := parseRoles(child.Children[1:])
				if err != nil {
					return nil, err
				}
				snapshot.Roles = roles
			case "permissions":
				perms, err := parsePermissions(child.Children[1:])
				if err != nil {
					return nil, err
				}
				snapshot.Permissions = perms
			case "workflows":
				wfs, err := parseWorkflows(child.Children[1:])
				if err != nil {
					return nil, err
				}
				snapshot.Workflows = wfs
			case "analytics-metrics":
				metrics, err := parseAnalyticsMetrics(child.Children[1:])
				if err != nil {
					return nil, err
				}
				snapshot.AnalyticsMetrics = metrics
			case "scripts":
				scripts, err := parseScriptSnapshots(child.Children[1:])
				if err != nil {
					return nil, err
				}
				snapshot.Scripts = scripts
			}
		} else if child.AtomType == sTokKeyword {
			// Config-level keyword
			switch child.Atom {
			case "min-kora-version":
				if i+1 < len(children) {
					snapshot.MinKoraVersion = nodeStr(children[i+1])
					i++
				}
			}
		}
	}
	return snapshot, nil
}

// ---------------------------------------------------------------------------
// Entity parsers
// ---------------------------------------------------------------------------

func parseDocTypes(nodes []*sNode) ([]*DocType, error) {
	var result []*DocType
	for _, n := range nodes {
		dt, err := parseDocType(n)
		if err != nil {
			return nil, err
		}
		result = append(result, dt)
	}
	return result, nil
}

func parseDocType(node *sNode) (*DocType, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid doctype form")
	}
	head := nodeStr(node.Children[0])
	if head != "doctype" {
		return nil, fmt.Errorf("expected doctype, got %s", head)
	}

	dt := &DocType{
		Name: nodeStr(node.Children[1]),
	}

	// Process remaining children: keywords and sub-forms
	kw := make(map[string]string)
	for i := 2; i < len(node.Children); i++ {
		child := node.Children[i]
		if child.IsList {
			if len(child.Children) == 0 {
				continue
			}
			subHead := nodeStr(child.Children[0])
			switch subHead {
			case "field":
				f, err := parseField(child)
				if err != nil {
					return nil, fmt.Errorf("field in doctype %s: %w", dt.Name, err)
				}
				dt.Fields = append(dt.Fields, *f)
			case "doc-constraint":
				dc, err := parseDocConstraint(child)
				if err != nil {
					return nil, fmt.Errorf("doc-constraint in doctype %s: %w", dt.Name, err)
				}
				dt.DocConstraints = append(dt.DocConstraints, *dc)
			}
		} else if child.AtomType == sTokKeyword {
			// Collect keyword value
			if i+1 < len(node.Children) {
				kw[child.Atom] = nodeStr(node.Children[i+1])
				i++ // skip the value
			}
		}
	}

	// Map keywords to struct fields
	applyDocTypeKeywords(dt, kw)
	return dt, nil
}

func applyDocTypeKeywords(dt *DocType, kw map[string]string) {
	dt.Module = kw["module"]
	dt.IsSubmittable = kwBool(kw, "submittable")
	dt.IsChildTable = kwBool(kw, "child-table")
	dt.IsSingle = kwBool(kw, "single")
	dt.TrackChanges = kwBool(kw, "track-changes")
	dt.TitleField = kw["title-field"]
	dt.SearchFields = kw["search-fields"]
	dt.SortField = kw["sort-field"]
	dt.SortOrder = kw["sort-order"]
	dt.Description = kw["description"]
}

func parseField(node *sNode) (*Field, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid field form")
	}
	head := nodeStr(node.Children[0])
	if head != "field" {
		return nil, fmt.Errorf("expected field, got %s", head)
	}

	f := &Field{
		Fieldname: nodeStr(node.Children[1]),
	}

	kw := make(map[string]string)
	for i := 2; i < len(node.Children); i++ {
		child := node.Children[i]
		if child.IsList {
			if len(child.Children) > 0 && nodeStr(child.Children[0]) == "constraint" {
				c, err := parseConstraint(child)
				if err != nil {
					return nil, err
				}
				f.Constraints = append(f.Constraints, *c)
			}
		} else if child.AtomType == sTokKeyword {
			if i+1 < len(node.Children) {
				kw[child.Atom] = nodeStr(node.Children[i+1])
				i++
			}
		}
	}

	applyFieldKeywords(f, kw)
	return f, nil
}

func applyFieldKeywords(f *Field, kw map[string]string) {
	f.Fieldtype = symbolOrString(kw, "type")
	f.Label = kw["label"]
	if v, ok := kw["options"]; ok {
		f.Options = v
	}
	if v, ok := kw["to"]; ok {
		f.Options = v // Link field target
	}
	f.Reqd = kwBool(kw, "required")
	f.Unique = kwBool(kw, "unique")
	f.Default = kw["default"]
	f.Hidden = kwBool(kw, "hidden")
	f.ReadOnly = kwBool(kw, "read-only")
	f.Bold = kwBool(kw, "bold")
	f.InListView = kwBool(kw, "in-list-view")
	f.InStandardFilter = kwBool(kw, "in-standard-filter")
	f.SearchIndex = kwBool(kw, "search-index")
	f.Description = kw["description"]
	f.DependsOn = kw["depends-on"]
	f.MandatoryDependsOn = kw["mandatory-depends-on"]
	f.RenamedFrom = kw["renamed-from"]
	f.LinkedField = kw["linked-field"]
	f.Computed = kw["computed"]
	f.DependencyScope = kw["dependency-scope"]
}

func parseConstraint(node *sNode) (*Constraint, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid constraint form")
	}
	head := nodeStr(node.Children[0])
	if head != "constraint" {
		return nil, fmt.Errorf("expected constraint, got %s", head)
	}

	c := &Constraint{Type: nodeStr(node.Children[1])}

	// Parse keyword args
	for i := 2; i < len(node.Children); i++ {
		child := node.Children[i]
		if child.AtomType == sTokKeyword {
			if i+1 < len(node.Children) {
				valNode := node.Children[i+1]
				val := nodeStr(valNode)
				switch child.Atom {
				case "value":
					// Try to parse as int first, then float, then keep as string
					if n, err := strconv.ParseInt(val, 10, 64); err == nil {
						c.Value = n
					} else if f, err := strconv.ParseFloat(val, 64); err == nil {
						c.Value = f
					} else {
						c.Value = val
					}
				case "values":
					c.Values = splitSpace(val)
				case "pattern":
					c.Pattern = val
				case "message":
					c.Message = val
				case "condition":
					c.Condition = val
				case "scope":
					c.Scope = val
				}
				i++
			}
		}
	}
	return c, nil
}

func parseDocConstraint(node *sNode) (*DocConstraint, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid doc-constraint form")
	}
	head := nodeStr(node.Children[0])
	if head != "doc-constraint" {
		return nil, fmt.Errorf("expected doc-constraint, got %s", head)
	}

	dc := &DocConstraint{Type: nodeStr(node.Children[1])}

	for i := 2; i < len(node.Children); i++ {
		child := node.Children[i]
		if child.AtomType == sTokKeyword {
			if i+1 < len(node.Children) {
				val := nodeStr(node.Children[i+1])
				switch child.Atom {
				case "description":
					dc.Description = val
				case "condition":
					dc.Condition = val
				case "require-fields":
					dc.RequireFields = splitSpace(val)
				case "field":
					dc.Field = val
				case "group-by":
					dc.GroupBy = splitSpace(val)
				case "max":
					if m, err := strconv.ParseFloat(val, 64); err == nil {
						dc.Max = m
					}
				case "message":
					dc.Message = val
				case "lhs":
					dc.LHS = val
				case "operator":
					dc.Operator = val
				case "rhs":
					dc.RHS = val
				case "fields":
					dc.Fields = splitSpace(val)
				case "status-field":
					dc.StatusField = val
				case "status-values":
					dc.StatusValues = splitSpace(val)
				case "immutable-fields":
					dc.ImmutableFields = splitSpace(val)
				case "constraints":
					for _, t := range splitSpace(val) {
						dc.Constraints = append(dc.Constraints, Constraint{Type: t})
					}
				}
				i++
			}
		}
	}
	return dc, nil
}

func parseRoles(nodes []*sNode) ([]*Role, error) {
	var result []*Role
	for _, n := range nodes {
		r, err := parseRole(n)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}

func parseRole(node *sNode) (*Role, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid role form")
	}
	if nodeStr(node.Children[0]) != "role" {
		return nil, fmt.Errorf("expected role, got %s", nodeStr(node.Children[0]))
	}

	r := &Role{Name: nodeStr(node.Children[1])}
	kw := extractKeywords(node.Children[2:])
	r.WorkspaceAccess = kwBool(kw, "workspace")
	r.Description = kw["description"]
	return r, nil
}

func parsePermissions(nodes []*sNode) ([]*Permission, error) {
	var result []*Permission
	for _, n := range nodes {
		p, err := parsePermission(n)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, nil
}

func parsePermission(node *sNode) (*Permission, error) {
	if !node.IsList || len(node.Children) < 3 {
		return nil, fmt.Errorf("invalid perm form")
	}
	if nodeStr(node.Children[0]) != "perm" {
		return nil, fmt.Errorf("expected perm, got %s", nodeStr(node.Children[0]))
	}

	p := &Permission{
		Doctype: nodeStr(node.Children[1]),
		Role:    nodeStr(node.Children[2]),
	}
	kw := extractKeywords(node.Children[3:])
	p.Read = kwBool(kw, "read")
	p.Write = kwBool(kw, "write")
	p.Create = kwBool(kw, "create")
	p.Delete = kwBool(kw, "delete")
	p.Submit = kwBool(kw, "submit")
	p.Cancel = kwBool(kw, "cancel")
	p.Amend = kwBool(kw, "amend")
	p.Export = kwBool(kw, "export")
	p.Import = kwBool(kw, "import")
	p.Report = kwBool(kw, "report")
	p.IfOwner = kwBool(kw, "if-owner")
	return p, nil
}

func parseWorkflows(nodes []*sNode) ([]*Workflow, error) {
	var result []*Workflow
	for _, n := range nodes {
		wf, err := parseWorkflow(n)
		if err != nil {
			return nil, err
		}
		result = append(result, wf)
	}
	return result, nil
}

func parseWorkflow(node *sNode) (*Workflow, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid workflow form")
	}
	if nodeStr(node.Children[0]) != "workflow" {
		return nil, fmt.Errorf("expected workflow, got %s", nodeStr(node.Children[0]))
	}

	wf := &Workflow{Name: nodeStr(node.Children[1])}
	wf.WorkflowStateField = "status" // default

	kw := make(map[string]string)
	for i := 2; i < len(node.Children); i++ {
		child := node.Children[i]
		if child.IsList {
			if len(child.Children) == 0 {
				continue
			}
			subHead := nodeStr(child.Children[0])
			switch subHead {
			case "state":
				s, err := parseWorkflowState(child)
				if err != nil {
					return nil, err
				}
				wf.States = append(wf.States, *s)
			case "action":
				t, err := parseWorkflowTransition(child)
				if err != nil {
					return nil, err
				}
				wf.Transitions = append(wf.Transitions, *t)
			case "notification":
				n, err := parseWorkflowNotification(child)
				if err != nil {
					return nil, err
				}
				wf.Notifications = append(wf.Notifications, *n)
			}
		} else if child.AtomType == sTokKeyword {
			if i+1 < len(node.Children) {
				kw[child.Atom] = nodeStr(node.Children[i+1])
				i++
			}
		}
	}

	if v, ok := kw["on"]; ok {
		wf.DocumentType = v
	}
	if v, ok := kw["active"]; ok {
		wf.IsActive = v == "true"
	}
	if v, ok := kw["state-field"]; ok {
		wf.WorkflowStateField = v
	}

	return wf, nil
}

func parseWorkflowState(node *sNode) (*WorkflowState, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid state form")
	}
	s := &WorkflowState{State: nodeStr(node.Children[1])}
	kw := extractKeywords(node.Children[2:])
	s.DocStatus = kwInt(kw, "doc-status")
	s.AllowEdit = kw["allow-edit"]
	s.Style = kw["style"]
	return s, nil
}

func parseWorkflowTransition(node *sNode) (*WorkflowTransition, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid action form")
	}
	t := &WorkflowTransition{Action: nodeStr(node.Children[1])}
	kw := make(map[string]string)
	for i := 2; i < len(node.Children); i++ {
		child := node.Children[i]
		if child.IsList {
			if len(child.Children) == 0 {
				continue
			}
			subHead := nodeStr(child.Children[0])
			var wa *WorkflowAction
			switch subHead {
			case "on-transition":
				wa = parseWorkflowActionInner(child)
				if wa != nil {
					t.OnTransition = append(t.OnTransition, *wa)
				}
			case "on-success":
				wa = parseWorkflowActionInner(child)
				if wa != nil {
					t.OnSuccess = append(t.OnSuccess, *wa)
				}
			case "on-failure":
				wa = parseWorkflowActionInner(child)
				if wa != nil {
					t.OnFailure = append(t.OnFailure, *wa)
				}
			}
		} else if child.AtomType == sTokKeyword {
			if i+1 < len(node.Children) {
				kw[child.Atom] = nodeStr(node.Children[i+1])
				i++
			}
		}
	}
	t.From = kw["from"]
	t.To = kw["to"]
	t.Allowed = kw["allowed"]
	t.Condition = kw["condition"]
	if v, ok := kw["require-fields"]; ok {
		t.RequireFields = splitSpace(v)
	}
	return t, nil
}

func parseWorkflowActionInner(node *sNode) *WorkflowAction {
	if len(node.Children) < 1 {
		return nil
	}
	wa := &WorkflowAction{}
	kw := extractKeywords(node.Children[1:])
	wa.Type = kw["type"]
	wa.Script = kw["script"]
	wa.WebhookURL = kw["webhook-url"]
	wa.Condition = kw["condition"]
	wa.Async = kwBool(kw, "async")
	return wa
}

func parseWorkflowNotification(node *sNode) (*WorkflowNotification, error) {
	if len(node.Children) < 1 {
		return nil, fmt.Errorf("invalid notification form")
	}
	n := &WorkflowNotification{}
	kw := make(map[string]string)
	for i := 1; i < len(node.Children); i++ {
		child := node.Children[i]
		if child.AtomType == sTokKeyword {
			if child.Atom == "recipients" {
				if i+1 < len(node.Children) && node.Children[i+1].IsList {
					n.Recipients = parseRecipients(node.Children[i+1])
					i++
				}
			} else if i+1 < len(node.Children) {
				kw[child.Atom] = nodeStr(node.Children[i+1])
				i++
			}
		}
	}
	n.Event = kw["event"]
	n.ToState = kw["to-state"]
	n.Subject = kw["subject"]
	n.Message = kw["message"]
	return n, nil
}

func parseRecipients(node *sNode) []map[string]string {
	var result []map[string]string
	for _, child := range node.Children {
		if child.IsList && len(child.Children) >= 2 {
			m := make(map[string]string)
			// Inner list is (key value) pairs
			for j := 0; j+1 < len(child.Children); j += 2 {
				m[nodeStr(child.Children[j])] = nodeStr(child.Children[j+1])
			}
			if len(m) > 0 {
				result = append(result, m)
			}
		}
	}
	return result
}

func parseAnalyticsMetrics(nodes []*sNode) ([]*AnalyticsMetricConfig, error) {
	var result []*AnalyticsMetricConfig
	for _, n := range nodes {
		m, err := parseAnalyticsMetric(n)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, nil
}

func parseAnalyticsMetric(node *sNode) (*AnalyticsMetricConfig, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid metric form")
	}
	if nodeStr(node.Children[0]) != "metric" {
		return nil, fmt.Errorf("expected metric, got %s", nodeStr(node.Children[0]))
	}
	m := &AnalyticsMetricConfig{Name: nodeStr(node.Children[1])}
	kw := extractKeywords(node.Children[2:])
	m.Label = kw["label"]
	m.Type = symbolOrString(kw, "type")
	m.DocType = symbolOrString(kw, "doctype")
	m.FieldName = kw["field"]
	m.LinkField = kw["link-field"]
	m.GroupByField = kw["group-by"]
	m.AutoGenerated = kwBool(kw, "auto-generated")
	return m, nil
}

func parseScriptSnapshots(nodes []*sNode) ([]*ScriptSnapshot, error) {
	var result []*ScriptSnapshot
	for _, n := range nodes {
		s, err := parseScriptSnapshot(n)
		if err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, nil
}

func parseScriptSnapshot(node *sNode) (*ScriptSnapshot, error) {
	if !node.IsList || len(node.Children) < 2 {
		return nil, fmt.Errorf("invalid script-ref form")
	}
	if nodeStr(node.Children[0]) != "script-ref" {
		return nil, fmt.Errorf("expected script-ref, got %s", nodeStr(node.Children[0]))
	}
	s := &ScriptSnapshot{Name: nodeStr(node.Children[1])}
	kw := extractKeywords(node.Children[2:])
	s.ScriptType = symbolOrString(kw, "script-type")
	s.DocType = symbolOrString(kw, "doctype")
	s.Event = kw["event"]
	s.MethodPath = kw["method"]
	s.WorkflowAction = kw["action"]
	s.Schedule = kw["schedule"]
	s.Priority = kwInt(kw, "priority")
	s.IsActive = kwBool(kw, "active")
	s.RunAs = kw["run-as"]
	s.TimeoutMs = kwInt(kw, "timeout")
	s.ScriptHash = kw["hash"]
	return s, nil
}

// ---------------------------------------------------------------------------
// Parser helpers
// ---------------------------------------------------------------------------

// nodeStr returns the string value of a node (accepts symbols, strings, bools, numbers).
func nodeStr(n *sNode) string {
	if n.IsList {
		return ""
	}
	return n.Atom
}

// extractKeywords extracts keyword-value pairs from a list of nodes.
func extractKeywords(nodes []*sNode) map[string]string {
	kw := make(map[string]string)
	for i := 0; i < len(nodes); i++ {
		if nodes[i].AtomType == sTokKeyword {
			if i+1 < len(nodes) {
				kw[nodes[i].Atom] = nodeStr(nodes[i+1])
				i++
			}
		}
	}
	return kw
}

func kwBool(kw map[string]string, key string) bool {
	return kw[key] == "true"
}

func kwInt(kw map[string]string, key string) int {
	if v, ok := kw[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func symbolOrString(kw map[string]string, key string) string {
	return kw[key]
}

func splitSpace(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// ---------------------------------------------------------------------------
// CanonicalizeSExpr
// ---------------------------------------------------------------------------

// CanonicalizeSExpr re-parses and re-serializes an s-expression to ensure canonical form.
func CanonicalizeSExpr(sexpr string) string {
	snapshot, err := FromSExpr(sexpr)
	if err != nil {
		// If we can't parse, return as-is (might be a non-config s-expr)
		return sexpr
	}
	return ToSExpr(snapshot)
}
