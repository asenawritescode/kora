package doctype

import (
	"encoding/json"
	"fmt"
	"strings"
)

// writeViewsSection writes all views as a single s-expression section.
func writeViewsSection(b *strings.Builder, views []*View) {
	if len(views) == 0 {
		b.WriteString("\n  (views)")
		return
	}

	sorted := sortedViewsByName(views)

	b.WriteString("\n  (views")
	for _, v := range sorted {
		writeSExprView(b, v, "    ")
	}
	b.WriteString(")")
}

// writeSExprView serializes a single View as an s-expression.
func writeSExprView(b *strings.Builder, v *View, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(view ")
	b.WriteString(symbolName(v.Name))

	// Collect keyword props (deterministic via writeSortedProps).
	var props []sExprProp
	if v.Route != "" {
		props = append(props, sExprProp{"route", quoteSExprString(v.Route)})
	}
	if v.Type != "" {
		props = append(props, sExprProp{"type", quoteSExprString(v.Type)})
	}
	if v.Layout != "" {
		props = append(props, sExprProp{"layout", quoteSExprString(v.Layout)})
	}
	if v.Label != "" {
		props = append(props, sExprProp{"label", quoteSExprString(v.Label)})
	}
	if v.Module != "" {
		props = append(props, sExprProp{"module", quoteSExprString(v.Module)})
	}
	if v.SourceDocType != "" {
		props = append(props, sExprProp{"source-doctype", quoteSExprString(v.SourceDocType)})
	}
	if len(v.Resources) > 0 {
		if encoded, err := json.Marshal(v.Resources); err == nil {
			props = append(props, sExprProp{"resources-json", quoteSExprString(string(encoded))})
		}
	}
	if v.PageManifest != nil {
		if encoded, err := json.Marshal(v.PageManifest); err == nil {
			props = append(props, sExprProp{"page-manifest-json", quoteSExprString(string(encoded))})
		}
	}

	subIndent := indent + "  "
	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, subIndent, props)
	}

	// Write components.
	for _, comp := range v.Components {
		writeSExprViewComponent(b, &comp, subIndent)
	}

	// Write public access if enabled.
	if v.PublicAccess != nil && v.PublicAccess.Enabled {
		writeSExprViewPublicAccess(b, v.PublicAccess, subIndent)
	}

	b.WriteString(")")
}

// writeSExprViewComponent serializes a component tree.
func writeSExprViewComponent(b *strings.Builder, c *ViewComponent, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(component ")
	b.WriteString(symbolName(c.ID))

	var props []sExprProp
	props = append(props, sExprProp{"type", quoteSExprString(c.Type)})
	if c.Region != "" && c.Region != "main" {
		props = append(props, sExprProp{"region", quoteSExprString(c.Region)})
	}
	if c.Label != "" {
		props = append(props, sExprProp{"label", quoteSExprString(c.Label)})
	}
	if c.SourceDocType != "" {
		props = append(props, sExprProp{"source-doctype", quoteSExprString(c.SourceDocType)})
	}
	if c.Data != "" {
		props = append(props, sExprProp{"data", quoteSExprString(c.Data)})
	}
	if len(c.ResourceBindings) > 0 {
		if encoded, err := json.Marshal(c.ResourceBindings); err == nil {
			props = append(props, sExprProp{"resource-bindings-json", quoteSExprString(string(encoded))})
		}
	}
	if c.Span > 0 {
		props = append(props, sExprProp{"span", fmt.Sprintf("%d", c.Span)})
	}

	subIndent := indent + "  "

	// Bindings — as child list for structured parsing.
	if len(c.Bindings) > 0 {
		writeSExprBindingsList(b, c.Bindings, subIndent)
	}

	// Filters — as child list for structured parsing.
	if len(c.Filters) > 0 {
		b.WriteString("\n")
		b.WriteString(subIndent)
		b.WriteString("(filters")
		for _, f := range c.Filters {
			b.WriteString(fmt.Sprintf(" (filter :field %s :op %s :value %s)",
				quoteSExprString(f.Field), quoteSExprString(f.Op), quoteSExprAny(f.Value)))
		}
		b.WriteString(")")
	}

	// Actions — as child list for structured parsing.
	for _, a := range c.Actions {
		b.WriteString("\n")
		b.WriteString(subIndent)
		b.WriteString(fmt.Sprintf("(action %s :trigger %s :type %s",
			symbolName(a.ID), quoteSExprString(a.Trigger), quoteSExprString(a.Type)))
		if len(a.Config) > 0 {
			configStr, _ := json.Marshal(a.Config)
			b.WriteString(fmt.Sprintf(" :config %s", quoteSExprString(string(configStr))))
		}
		b.WriteString(")")
	}

	// Rules — as child list for structured parsing.
	for _, r := range c.Rules {
		b.WriteString("\n")
		b.WriteString(subIndent)
		b.WriteString(fmt.Sprintf("(rule :target %s :field %s :op %s",
			quoteSExprString(r.Target), quoteSExprString(r.Condition.Field),
			quoteSExprString(r.Condition.Op)))
		if r.Condition.Value != nil {
			b.WriteString(fmt.Sprintf(" :value %s", quoteSExprAny(r.Condition.Value)))
		}
		b.WriteString(")")
	}

	// Columns — as child list for structured parsing.
	if len(c.DesktopColumns) > 0 {
		b.WriteString("\n")
		b.WriteString(subIndent)
		b.WriteString("(desktop-columns")
		for _, col := range c.DesktopColumns {
			b.WriteString(" ")
			b.WriteString(quoteSExprString(col))
		}
		b.WriteString(")")
	}
	if len(c.MobileColumns) > 0 {
		b.WriteString("\n")
		b.WriteString(subIndent)
		b.WriteString("(mobile-columns")
		for _, col := range c.MobileColumns {
			b.WriteString(" ")
			b.WriteString(quoteSExprString(col))
		}
		b.WriteString(")")
	}

	if len(props) > 0 {
		b.WriteString("\n")
		writeSortedProps(b, subIndent, props)
	}

	// Nested children.
	for _, child := range c.Components {
		writeSExprViewComponent(b, &child, subIndent)
	}

	b.WriteString(")")
}

func writeSExprViewPublicAccess(b *strings.Builder, pa *ViewPublicAccess, indent string) {
	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(public-access :components ")
	compJSON, _ := json.Marshal(pa.Components)
	b.WriteString(quoteSExprString(string(compJSON)))
	if pa.AllowMutations {
		b.WriteString(" :allow-mutations true")
	}
	b.WriteString(")")
}

// writeSExprBindingsList writes bindings as a list of (key value) pairs,
// sorted by key for deterministic output.
func writeSExprBindingsList(b *strings.Builder, bindings map[string]string, indent string) {
	keys := make([]string, 0, len(bindings))
	for k := range bindings {
		keys = append(keys, k)
	}
	sortStrings(keys)

	b.WriteString("\n")
	b.WriteString(indent)
	b.WriteString("(bindings")
	for _, k := range keys {
		b.WriteString(fmt.Sprintf(" (%s %s)", symbolName(k), quoteSExprString(bindings[k])))
	}
	b.WriteString(")")
}

func quoteSExprAny(v any) string {
	switch val := v.(type) {
	case string:
		return quoteSExprString(val)
	case json.Number:
		return string(val)
	case float64:
		return fmt.Sprintf("%v", val)
	case bool:
		return fmt.Sprintf("%v", val)
	case int:
		return fmt.Sprintf("%d", val)
	default:
		return quoteSExprString(fmt.Sprintf("%v", v))
	}
}

// sortedViewsByName sorts views by name for deterministic output.
func sortedViewsByName(views []*View) []*View {
	result := make([]*View, len(views))
	copy(result, views)
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].Name < result[j-1].Name; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}
