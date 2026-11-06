package site

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
)

var (
	inputRef   = regexp.MustCompile(`\$input\.([A-Za-z_][A-Za-z0-9_]*)`)
	forEachRef = regexp.MustCompile(`"for_each":"\$input\.([A-Za-z_][A-Za-z0-9_]*)"`)
)

// addPackCommands publishes every owner-tagged command of the snapshot as an
// operation of core and requires its capability from app. It fills the
// command's schema refs in snapshot.Commands.
func addPackCommands(core, app *contract.ModuleDescriptor, snapshot *doctype.ConfigSnapshot) error {
	queryCaps := map[contract.CapabilityName]bool{}
	for _, o := range core.Provides {
		queryCaps[o.Name] = true
	}
	records := map[string]*doctype.DocType{}
	for _, dt := range snapshot.DocTypes {
		records[dt.Name] = dt
	}
	offers := map[contract.CapabilityName]*contract.CapabilityOffer{}
	var names []contract.CapabilityName
	for i, raw := range snapshot.Commands {
		cmd, err := kernel.ParseCommandResourceDraftJSON(raw)
		if err != nil {
			return fmt.Errorf("pack.command_invalid: %w", err)
		}
		if cmd.ModuleOwnerID == "" {
			continue
		}
		if cmd.ModuleOwnerID != core.ID {
			return fmt.Errorf("pack.module_owner_mismatch: command %q module_owner_id %q must be %q", cmd.Name, cmd.ModuleOwnerID, core.ID)
		}
		if queryCaps[cmd.Capability] {
			return fmt.Errorf("pack.capability_mixed: command %q capability %q is also a query capability", cmd.Name, cmd.Capability)
		}
		version := uint32(cmd.Version)
		if version == 0 {
			version = 1
		}
		cmd.InputSchema = contract.SchemaRef{ID: fmt.Sprintf("%s.%s.input", core.ID, cmd.Operation), Version: version}
		cmd.OutputSchema = contract.SchemaRef{ID: fmt.Sprintf("%s.%s.output", core.ID, cmd.Operation), Version: version}
		if snapshot.Commands[i], err = json.Marshal(cmd); err != nil {
			return err
		}
		core.Schemas = append(core.Schemas,
			contract.ModuleSchemaDocument{Ref: cmd.InputSchema, Schema: commandInputSchema(cmd, records)},
			contract.ModuleSchemaDocument{Ref: cmd.OutputSchema, Schema: commandOutputSchema(cmd)})
		o := offers[cmd.Capability]
		if o == nil {
			o = &contract.CapabilityOffer{Name: cmd.Capability, Contract: contract.SchemaRef{ID: string(cmd.Capability) + ".contract", Version: 1}}
			offers[cmd.Capability] = o
			names = append(names, cmd.Capability)
		}
		o.Operations = append(o.Operations, contract.OperationSpec{Name: cmd.Operation, Input: cmd.InputSchema, Output: cmd.OutputSchema, Effect: contract.EffectLocal})
		po := contract.PublishedOperation{Name: cmd.Operation, Capability: cmd.Capability, Operation: cmd.Operation, ScopePolicy: contract.ScopeTenantShared,
			AllowedCallers: []contract.ModuleID{app.ID}, OfflineEligible: cmd.OfflineEligible}
		if cmd.Presentation != nil {
			po.Presentation = contract.SurfacePresentation(*cmd.Presentation)
		}
		if po.Payment, err = packPaymentBinding(cmd, records); err != nil {
			return err
		}
		core.PublishedOperations = append(core.PublishedOperations, po)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	for _, n := range names {
		core.Provides = append(core.Provides, *offers[n])
		app.Requires = append(app.Requires, contract.CapabilityRequirement{Name: n, Contract: offers[n].Contract, Required: true})
	}
	return nil
}

func fieldSchema(f doctype.Field, records map[string]*doctype.DocType) map[string]any {
	switch f.Fieldtype {
	case "Int":
		return map[string]any{"type": "integer"}
	case "Float", "Currency", "Percent":
		return map[string]any{"type": "number"}
	case "Check":
		return map[string]any{"type": []string{"integer", "boolean"}}
	case "Table":
		items := map[string]any{"type": "object"}
		if child := records[f.Options]; child != nil {
			props := map[string]any{}
			for _, cf := range child.Fields {
				props[cf.Fieldname] = fieldSchema(cf, records)
			}
			items["properties"] = props
		}
		return map[string]any{"type": "array", "items": items}
	case "Select":
		out := map[string]any{"type": "string"}
		var enum []string
		for _, o := range strings.Split(f.Options, "\n") {
			if o = strings.TrimSpace(o); o != "" {
				enum = append(enum, o)
			}
		}
		if len(enum) > 0 {
			out["enum"] = enum
		}
		return out
	case "Data", "Link", "Date", "Datetime", "Text", "Small Text", "Long Text":
		return map[string]any{"type": "string"}
	}
	return map[string]any{}
}

func commandInputSchema(cmd *kernel.CommandResource, records map[string]*doctype.DocType) json.RawMessage {
	raw, _ := json.Marshal(cmd)
	fields := map[string]doctype.Field{}
	if dt := records[cmd.Input.Record]; dt != nil {
		for _, f := range dt.Fields {
			fields[f.Fieldname] = f
		}
	}
	props := map[string]any{}
	required := map[string]bool{}
	for _, m := range inputRef.FindAllSubmatch(raw, -1) {
		name := string(m[1])
		if extra, ok := cmd.Input.Extra[name]; ok {
			props[name] = map[string]any{"type": extra.Type}
			required[name] = extra.Required
		} else if f, ok := fields[name]; ok {
			props[name] = fieldSchema(f, records)
			required[name] = f.Reqd && f.Default == "" && f.Computed == "" && f.LinkedField == ""
		} else {
			props[name] = map[string]any{}
		}
	}
	for _, name := range cmd.RequiredInputs() {
		required[name] = true
	}
	for _, m := range forEachRef.FindAllSubmatch(raw, -1) {
		if _, ok := props[string(m[1])]; !ok {
			props[string(m[1])] = map[string]any{"type": "array"}
		}
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	var req []string
	for name, r := range required {
		if r {
			req = append(req, name)
		}
	}
	if len(req) > 0 {
		sort.Strings(req)
		schema["required"] = req
	}
	out, _ := json.Marshal(schema)
	return out
}

func commandOutputSchema(cmd *kernel.CommandResource) json.RawMessage {
	declared := map[string]any{}
	for k := range cmd.Output {
		declared[k] = map[string]any{}
	}
	out, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{
		"command": map[string]any{"type": "string"},
		"steps":   map[string]any{"type": "array"},
		"output":  map[string]any{"type": "object", "properties": declared},
	}})
	return out
}

// packPaymentBinding derives the published payment descriptor of a command with
// a payment guard from the guard and input.record. It is nil without a guard.
func packPaymentBinding(cmd *kernel.CommandResource, records map[string]*doctype.DocType) (*contract.PaymentOperationContract, error) {
	for _, g := range cmd.Require {
		if g.Payment == nil {
			continue
		}
		field := func(ref string) string { return ref[strings.IndexByte(ref, '.')+1:] }
		p := &contract.PaymentOperationContract{SourceDoctype: cmd.Input.Record, LinesField: field(g.Payment.ForEach),
			ReferenceField: field(g.Payment.Reference), AmountField: field(g.Payment.Amount), CurrencyField: field(g.Payment.Currency)}
		dt := records[cmd.Input.Record]
		if dt == nil {
			return nil, fmt.Errorf("pack.payment_binding_invalid: command %q input.record %q is not in the pack", cmd.Name, cmd.Input.Record)
		}
		for _, f := range dt.Fields {
			if f.Fieldname == p.LinesField && f.Fieldtype == "Table" {
				return p, nil
			}
		}
		return nil, fmt.Errorf("pack.payment_binding_invalid: command %q lines field %q is not a Table field of %q", cmd.Name, p.LinesField, cmd.Input.Record)
	}
	return nil, nil
}
