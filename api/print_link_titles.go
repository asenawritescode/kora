package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

// maxLinkTitleNames bounds the distinct link values resolved per target for one
// printed record.
const maxLinkTitleNames = 500

// linkTitleLookup returns the title of each named record of target. Names it
// cannot resolve are simply absent from the result.
type linkTitleLookup func(target *doctype.DocType, names []string) (map[string]string, error)

// addLinkTitles gives every non-empty Link value of a printed record, and of
// its child rows, a "<field>_title" sibling holding the target record's title
// (its DocType title_field). A link that cannot be resolved gets its own value
// so a template never prints a blank. An existing key is left alone.
func addLinkTitles(doc map[string]any, dt *doctype.DocType, reg *doctype.Registry, lookup linkTitleLookup) error {
	type slot struct {
		row    map[string]any
		key    string
		target *doctype.DocType
		value  string
	}
	var slots []slot
	var walk func(row map[string]any, dt *doctype.DocType)
	walk = func(row map[string]any, dt *doctype.DocType) {
		if row == nil || dt == nil {
			return
		}
		for _, f := range dt.DataFields() {
			switch f.Fieldtype {
			case "Link":
				value, _ := row[f.Fieldname].(string)
				key := f.Fieldname + "_title"
				if _, taken := row[key]; value == "" || taken {
					continue
				}
				slots = append(slots, slot{row, key, reg.Get(strings.TrimSpace(f.Options)), value})
			case "Table":
				child := reg.Get(strings.TrimSpace(f.Options))
				switch rows := row[f.Fieldname].(type) {
				case []map[string]any:
					for _, r := range rows {
						walk(r, child)
					}
				case []any:
					for _, r := range rows {
						if m, ok := r.(map[string]any); ok {
							walk(m, child)
						}
					}
				}
			}
		}
	}
	walk(doc, dt)

	wanted := map[string][]string{}
	targets := map[string]*doctype.DocType{}
	seen := map[string]bool{}
	for _, s := range slots {
		if s.target == nil || seen[s.target.Name+"\x00"+s.value] || len(wanted[s.target.Name]) >= maxLinkTitleNames {
			continue
		}
		seen[s.target.Name+"\x00"+s.value] = true
		wanted[s.target.Name] = append(wanted[s.target.Name], s.value)
		targets[s.target.Name] = s.target
	}
	titles := map[string]map[string]string{}
	for name, names := range wanted {
		found, err := lookup(targets[name], names)
		if err != nil {
			return err
		}
		titles[name] = found
	}
	for _, s := range slots {
		title := s.value
		if s.target != nil && titles[s.target.Name][s.value] != "" {
			title = titles[s.target.Name][s.value]
		}
		s.row[s.key] = title
	}
	return nil
}

// printLinkTitles adds link titles to a printed record. Only the target's
// title_field is read, and for any actor who may print the record: the title is
// the label of a link the record already shows. Private module resources,
// entity-owned targets and hidden or sensitive title fields are never read;
// their links print their stored value.
func (s sitePrintSource) printLinkTitles(c *gin.Context, doc map[string]any, dt *doctype.DocType) error {
	registry := s.h.siteRegistry(c)
	tx := s.h.siteTx(c)
	return addLinkTitles(doc, dt, registry, func(target *doctype.DocType, names []string) (map[string]string, error) {
		title, ok := titleReadable(target, registry)
		if !ok {
			return nil, nil
		}
		filter, err := json.Marshal([][]any{{"name", "in", names}})
		if err != nil {
			return nil, err
		}
		docs, _, err := tx.GetListWithScope(target, string(filter), "name", len(names), 0, policy.RowScope{}, []string{title}, "")
		if err != nil {
			return nil, fmt.Errorf("resolving %s titles: %w", target.Name, err)
		}
		out := make(map[string]string, len(docs))
		for _, d := range docs {
			if v, ok := d.Get(title).(string); ok {
				out[d.Name] = v
			}
		}
		return out, nil
	})
}

func titleReadable(target *doctype.DocType, reg *doctype.Registry) (string, bool) {
	title := strings.TrimSpace(target.TitleField)
	f := target.GetField(title)
	switch {
	case title == "" || title == "name" || f == nil || f.Hidden || strings.EqualFold(f.Fieldtype, "Password"):
		return "", false
	case reg.IsPrivateModuleResource(target.Name) || target.BusinessScopePolicy == contract.ScopeEntityOwned:
		return "", false
	}
	return title, true
}
