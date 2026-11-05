package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// importUnit is one bulk input: a single CSV row, or a group of consecutive
// rows. First is the stable row key.
type importUnit struct {
	First, Last int
	Data        map[string]any
	Err         *contract.Error
}

func importUnitError(msg string, row int, field string) *contract.Error {
	e := contract.NewError(contract.CodeValidationFailed, msg)
	e.Details, _ = json.Marshal(map[string]any{"row": row, "field": field})
	return e
}

// groupMapping splits a mapping into header and child targets, validating
// them against dt. It returns the single child Table field name (or "").
func groupMapping(reg *doctype.Registry, dt *doctype.DocType, mapping map[string]string) (head map[string]string, child map[string]string, table string, err error) {
	head, child = map[string]string{}, map[string]string{}
	var childDT *doctype.DocType
	for csvCol, target := range mapping {
		tbl, fld, dotted := strings.Cut(target, ".")
		if !dotted {
			if dt.GetField(target) == nil {
				return nil, nil, "", fmt.Errorf("mapping column or field unknown: %s", csvCol)
			}
			head[csvCol] = target
			continue
		}
		tf := dt.GetField(tbl)
		if tf == nil || tf.Fieldtype != "Table" || (table != "" && table != tbl) {
			return nil, nil, "", fmt.Errorf("mapping must use one Table field of the target: %s", target)
		}
		table = tbl
		if childDT = reg.Get(tf.Options); childDT == nil || childDT.GetField(fld) == nil {
			return nil, nil, "", fmt.Errorf("mapping child field unknown: %s", target)
		}
		child[csvCol] = fld
	}
	return head, child, table, nil
}

// assembleImportGroups turns CSV rows into one unit per run of consecutive
// rows sharing the group_by value. dt types the header fields; childDT the
// child rows. A repeated value after another is import.group_not_contiguous,
// a conflicting header cell import.group_inconsistent.
func assembleImportGroups(dt, childDT *doctype.DocType, table string, head, child map[string]string, col map[string]int, groupCol string, rows [][]string) []importUnit {
	cell := func(rec []string, name string) string {
		if ci := col[name]; ci < len(rec) {
			return rec[ci]
		}
		return ""
	}
	var units []importUnit
	seen := map[string]bool{}
	var cur *importUnit
	var curVal string
	var kids []map[string]any
	flush := func() {
		if cur != nil && cur.Err == nil && len(kids) > 0 {
			cur.Data[table] = kids
		}
		kids = nil
	}
	for i, rec := range rows {
		idx := i + 1
		g := strings.TrimSpace(cell(rec, groupCol))
		if cur == nil || g != curVal || g == "" {
			flush()
			units = append(units, importUnit{First: idx, Last: idx, Data: map[string]any{}})
			cur, curVal = &units[len(units)-1], g
			switch {
			case g == "":
				cur.Err = importUnitError("import.key_missing: empty group value "+groupCol, idx, groupCol)
			case seen[g]:
				cur.Err = importUnitError("import.group_not_contiguous: "+g+" repeats after another group", idx, groupCol)
			}
			seen[g] = true
		}
		cur.Last = idx
		if cur.Err != nil {
			continue
		}
		for csvCol, field := range head {
			v, e := parseImportValue(dt.GetField(field), cell(rec, csvCol))
			if e != nil {
				cur.Err = importUnitError(e.Error(), idx, field)
				break
			}
			if old, ok := cur.Data[field]; ok && v != nil && fmt.Sprint(old) != fmt.Sprint(v) {
				cur.Err = importUnitError("import.group_inconsistent: "+field+" differs within group "+g, idx, field)
				break
			}
			if v != nil {
				cur.Data[field] = v
			}
		}
		if cur.Err != nil {
			continue
		}
		row := map[string]any{}
		for csvCol, field := range child {
			v, e := parseImportValue(childDT.GetField(field), cell(rec, csvCol))
			if e != nil {
				cur.Err = importUnitError(e.Error(), idx, table+"."+field)
				break
			}
			if v != nil {
				row[field] = v
			}
		}
		if cur.Err == nil && len(row) > 0 {
			kids = append(kids, row)
		}
	}
	flush()
	return units
}

func (u importUnit) lastIfGrouped(groupBy string) int {
	if groupBy == "" {
		return 0
	}
	return u.Last
}

// assembleImportRows is the ungrouped path: one unit per CSV row.
func assembleImportRows(dt *doctype.DocType, mapping map[string]string, col map[string]int, rows [][]string) []importUnit {
	units := make([]importUnit, 0, len(rows))
	for i, rec := range rows {
		u := importUnit{First: i + 1, Last: i + 1, Data: map[string]any{}}
		for csvCol, field := range mapping {
			var cell string
			if ci := col[csvCol]; ci < len(rec) {
				cell = rec[ci]
			}
			v, e := parseImportValue(dt.GetField(field), cell)
			if e != nil {
				u.Err = importUnitError(e.Error(), u.First, field)
				u.Err.Details, _ = json.Marshal(map[string]string{"field": field})
				break
			}
			if v != nil {
				u.Data[field] = v
			}
		}
		units = append(units, u)
	}
	return units
}
