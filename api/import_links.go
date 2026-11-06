package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// linkLookup returns, for each value, the names of target records whose field
// ("name" or the target's title field) equals it, keyed by the lower-cased value.
type linkLookup func(target *doctype.DocType, field string, values []string) (map[string][]string, error)

type importLinkSlot struct {
	unit   *importUnit
	target *doctype.DocType
	field  string // reported field path
	value  string
	row    map[string]any
	key    string
}

// resolveImportLinks rewrites Link cells holding a plain label to the linked
// record's name. A value that is already a record name is kept; otherwise it is
// matched on the target's title_field. A value matching no record, or several,
// fails its row: an import never creates link targets and never stores a
// dangling link.
func resolveImportLinks(reg *doctype.Registry, dt *doctype.DocType, table string, units []importUnit, lookup linkLookup) error {
	var slots []importLinkSlot
	collect := func(u *importUnit, ft *doctype.DocType, row map[string]any, prefix string) {
		for _, f := range ft.Fields {
			s, ok := row[f.Fieldname].(string)
			target := reg.Get(f.Options)
			if f.Fieldtype == "Link" && ok && s != "" && target != nil {
				slots = append(slots, importLinkSlot{unit: u, target: target, field: prefix + f.Fieldname, value: s, row: row, key: f.Fieldname})
			}
		}
	}
	for i := range units {
		u := &units[i]
		if u.Err != nil {
			continue
		}
		collect(u, dt, u.Data, "")
		if kids, ok := u.Data[table].([]map[string]any); ok && table != "" {
			if childDT := reg.Get(dt.GetField(table).Options); childDT != nil {
				for _, kid := range kids {
					collect(u, childDT, kid, table+".")
				}
			}
		}
	}
	type resolution struct {
		name string
		err  string
	}
	resolved := map[string]resolution{}
	byTarget := map[string][]string{}
	targets := map[string]*doctype.DocType{}
	for _, s := range slots {
		k := s.target.Name + "\x00" + s.value
		if _, dup := resolved[k]; !dup {
			resolved[k] = resolution{}
			byTarget[s.target.Name] = append(byTarget[s.target.Name], s.value)
			targets[s.target.Name] = s.target
		}
	}
	for name, values := range byTarget {
		target := targets[name]
		byName, err := lookup(target, "name", values)
		if err != nil {
			return err
		}
		var rest []string
		for _, v := range values {
			if names := byName[strings.ToLower(v)]; len(names) == 1 {
				resolved[name+"\x00"+v] = resolution{name: names[0]}
			} else {
				rest = append(rest, v)
			}
		}
		var byTitle map[string][]string
		if f := target.GetField(target.TitleField); len(rest) > 0 && f != nil && f.Fieldname != "name" && !f.Hidden && !strings.EqualFold(f.Fieldtype, "Password") {
			if byTitle, err = lookup(target, f.Fieldname, rest); err != nil {
				return err
			}
		}
		for _, v := range rest {
			switch names := byTitle[strings.ToLower(v)]; len(names) {
			case 1:
				resolved[name+"\x00"+v] = resolution{name: names[0]}
			case 0:
				resolved[name+"\x00"+v] = resolution{err: fmt.Sprintf("import.link_unresolved: no %s named %q (create it first or fix the value)", name, v)}
			default:
				resolved[name+"\x00"+v] = resolution{err: fmt.Sprintf("import.link_ambiguous: %d %s records are titled %q; use the record name", len(names), name, v)}
			}
		}
	}
	for _, s := range slots {
		if s.unit.Err != nil {
			continue
		}
		r := resolved[s.target.Name+"\x00"+s.value]
		if r.err != "" {
			s.unit.Err = importUnitError(r.err, s.unit.First, s.field)
			s.unit.Err.Details, _ = json.Marshal(map[string]any{"row": s.unit.First, "field": s.field})
			continue
		}
		s.row[s.key] = r.name
	}
	return nil
}

// siteLinkLookup reads link targets as the caller: without read permission on
// the target no value can be resolved and the import is refused.
func (h *Handler) siteLinkLookup(c *gin.Context, reg *doctype.Registry) linkLookup {
	tx := h.siteTx(c)
	return func(target *doctype.DocType, field string, values []string) (map[string][]string, error) {
		scope, denial := permScope(c, reg, target.Name, "read")
		if denial != nil {
			return nil, contract.NewError(contract.CodePermissionDenied, "cannot read "+target.Name+" to resolve link values")
		}
		filter, err := json.Marshal([][]any{{field, "in", values}})
		if err != nil {
			return nil, err
		}
		var fields []string
		if field != "name" {
			fields = []string{field}
		}
		docs, _, err := tx.GetListWithScope(target, string(filter), "name", len(values)*4+10, 0, scope, fields, "")
		if err != nil {
			return nil, fmt.Errorf("resolving %s links: %w", target.Name, err)
		}
		out := map[string][]string{}
		for _, d := range docs {
			key := d.Name
			if field != "name" {
				key, _ = d.Get(field).(string)
			}
			out[strings.ToLower(key)] = append(out[strings.ToLower(key)], d.Name)
		}
		return out, nil
	}
}
