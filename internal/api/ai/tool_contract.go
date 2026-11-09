package ai

import (
	"github.com/asenawritescode/kora/contract"
)

// ToContractDescriptor projects an api/ai ToolDescriptor into the canonical
// versioned wire shape (contract.ToolDescriptor). This is the contract-parity
// bridge required by RFC §10.4.5: BuildToolCatalog remains the registry
// projection, and every adapter renders the canonical contract shape.
//
// The mapping preserves all shared, wire-visible fields. Safety levels are
// represented as strings in both packages so adapters can keep their existing
// vocabulary without changing the contract payload.
func ToContractDescriptor(d ToolDescriptor) contract.ToolDescriptor {
	fields := make([]contract.FieldHint, 0, len(d.FieldHints))
	for _, f := range d.FieldHints {
		fields = append(fields, contract.FieldHint{
			Name:           f.Name,
			Label:          f.Label,
			Fieldtype:      f.Fieldtype,
			Type:           f.Type,
			Format:         f.Format,
			Options:        f.Options,
			LinkTarget:     f.LinkTarget,
			TableTarget:    f.TableTarget,
			Required:       f.Required,
			ReadOnly:       f.ReadOnly,
			Computed:       f.Computed,
			Writable:       f.Writable,
			StandardFilter: f.StandardFilter,
			SearchIndex:    f.SearchIndex,
			InListView:     f.InListView,
			Unique:         f.Unique,
			Description:    f.Description,
		})
	}
	sys := make([]contract.SystemFieldHint, 0, len(d.SystemFields))
	for _, s := range d.SystemFields {
		sys = append(sys, contract.SystemFieldHint{Name: s.Name, Fieldtype: s.Fieldtype, Writable: s.Writable})
	}
	return contract.ToolDescriptor{
		ID:                      d.ID,
		Source:                  d.Source,
		Name:                    d.Name,
		Description:             d.Description,
		InputSchema:             d.InputSchema,
		SafetyLevel:             contract.ToolSafetyLevel(d.SafetyLevel),
		RequiresConfirmation:    d.RequiresConfirmation,
		RequiresRecentAuth:      d.RequiresRecentAuth,
		ChannelAllowlist:        d.ChannelAllowlist,
		ArgumentContractVersion: contract.ToolArgumentVersion(d.ArgumentContractVersion),
		Operation:               d.Operation,
		Doctype:                 d.Doctype,
		DoctypeLabel:            d.DoctypeLabel,
		TitleField:              d.TitleField,
		SearchFields:            d.SearchFields,
		SortField:               d.SortField,
		SortOrder:               d.SortOrder,
		FieldHints:              fields,
		SystemFields:            sys,
	}
}

// ToContractCatalog projects a ToolCatalog into the canonical shape.
func ToContractCatalog(c ToolCatalog) contract.ToolCatalog {
	tools := make([]contract.ToolDescriptor, 0, len(c.Tools))
	for _, t := range c.Tools {
		tools = append(tools, ToContractDescriptor(t))
	}
	return contract.ToolCatalog{Version: c.Version, Tools: tools}
}
