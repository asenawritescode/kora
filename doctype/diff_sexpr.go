package doctype

import (
	"sort"
	"strconv"
)

// ---------------------------------------------------------------------------
// DiffSExpr
// ---------------------------------------------------------------------------

// DiffSExpr compares two canonical s-expressions and returns the list of changes.
func DiffSExpr(oldSExpr, newSExpr string) ([]Change, error) {
	oldRoot, err := parseSExpr(oldSExpr)
	if err != nil {
		return nil, err
	}
	newRoot, err := parseSExpr(newSExpr)
	if err != nil {
		return nil, err
	}
	return diffConfig(oldRoot, newRoot), nil
}

// ---------------------------------------------------------------------------
// Config-level diff
// ---------------------------------------------------------------------------

func diffConfig(old, new *sNode) []Change {
	if !old.IsList || !new.IsList {
		return nil
	}
	var changes []Change

	// Config-level keyword args
	oldKWs := extractConfigKWs(old.Children[1:])
	newKWs := extractConfigKWs(new.Children[1:])

	for k, v := range oldKWs {
		if nv, ok := newKWs[k]; !ok {
			changes = append(changes, Change{
				Type:     "modify-config",
				Section:  "config",
				Entity:   k,
				OldValue: v,
			})
		} else if v != nv {
			changes = append(changes, Change{
				Type:     "modify-config",
				Section:  "config",
				Entity:   k,
				OldValue: v,
				NewValue: nv,
			})
		}
	}
	for k, v := range newKWs {
		if _, ok := oldKWs[k]; !ok {
			changes = append(changes, Change{
				Type:     "modify-config",
				Section:  "config",
				Entity:   k,
				NewValue: v,
			})
		}
	}

	// Collect sections
	oldSections := collectSections(old.Children[1:])
	newSections := collectSections(new.Children[1:])

	sectionOrder := []string{"doctypes", "roles", "permissions", "workflows", "analytics-metrics", "scripts"}
	for _, sec := range sectionOrder {
		oldSec, hasOld := oldSections[sec]
		newSec, hasNew := newSections[sec]
		if hasOld && hasNew {
			changes = append(changes, diffSection(sec, oldSec, newSec)...)
		} else if hasOld {
			changes = append(changes, removeAllSection(sec, oldSec)...)
		} else if hasNew {
			changes = append(changes, addAllSection(sec, newSec)...)
		}
	}

	return changes
}

func extractConfigKWs(children []*sNode) map[string]string {
	kw := make(map[string]string)
	for i := 0; i < len(children); i++ {
		if children[i].AtomType == sTokKeyword {
			if i+1 < len(children) {
				kw[children[i].Atom] = nodeStr(children[i+1])
				i++
			}
		}
	}
	return kw
}

func collectSections(children []*sNode) map[string]*sNode {
	sections := make(map[string]*sNode)
	for _, c := range children {
		if !c.IsList || len(c.Children) == 0 {
			continue
		}
		head := nodeStr(c.Children[0])
		switch head {
		case "doctypes", "roles", "permissions", "workflows", "analytics-metrics", "scripts":
			sections[head] = c
		}
	}
	return sections
}

func removeAllSection(section string, sec *sNode) []Change {
	var changes []Change
	for _, c := range subEntities(sec) {
		ename := entityNameOf(c)
		if ename == "" {
			continue
		}
		changes = append(changes, Change{
			Type:    "remove-" + section,
			Section: section,
			Entity:  ename,
		})
		changes = append(changes, removeSubEntity(section, ename, c)...)
	}
	return changes
}

func addAllSection(section string, sec *sNode) []Change {
	var changes []Change
	for _, c := range subEntities(sec) {
		ename := entityNameOf(c)
		if ename == "" {
			continue
		}
		changes = append(changes, Change{
			Type:    "add-" + section,
			Section: section,
			Entity:  ename,
			Attrs:   extractAttrs(c),
		})
		changes = append(changes, addSubEntity(section, ename, c)...)
	}
	return changes
}

// ---------------------------------------------------------------------------
// Section-level diff
// ---------------------------------------------------------------------------

func diffSection(section string, oldSec, newSec *sNode) []Change {
	return diffEntityList(section, subEntities(oldSec), subEntities(newSec))
}

func diffEntityList(section string, old, new []*sNode) []Change {
	var changes []Change

	oldMap := make(map[string]*sNode)
	newMap := make(map[string]*sNode)
	allNames := make(map[string]bool)

	for _, n := range old {
		ename := entityNameOf(n)
		if ename != "" {
			oldMap[ename] = n
			allNames[ename] = true
		}
	}
	for _, n := range new {
		ename := entityNameOf(n)
		if ename != "" {
			newMap[ename] = n
			allNames[ename] = true
		}
	}

	sortedNames := make([]string, 0, len(allNames))
	for n := range allNames {
		sortedNames = append(sortedNames, n)
	}
	sort.Strings(sortedNames)

	for _, ename := range sortedNames {
		oldN, hasOld := oldMap[ename]
		newN, hasNew := newMap[ename]
		if hasOld && hasNew {
			changes = append(changes, diffOneEntity(section, ename, oldN, newN)...)
		} else if hasOld {
			changes = append(changes, Change{
				Type:    "remove-" + section,
				Section: section,
				Entity:  ename,
			})
			changes = append(changes, removeSubEntity(section, ename, oldN)...)
		} else {
			changes = append(changes, Change{
				Type:    "add-" + section,
				Section: section,
				Entity:  ename,
				Attrs:   extractAttrs(newN),
			})
			changes = append(changes, addSubEntity(section, ename, newN)...)
		}
	}

	return changes
}

// ---------------------------------------------------------------------------
// Single entity comparison
// ---------------------------------------------------------------------------

func diffOneEntity(section, ename string, old, new *sNode) []Change {
	oldKW := extractAttrs(old)
	newKW := extractAttrs(new)

	var changes []Change

	diffKW := diffKeywordMaps(oldKW, newKW)
	if len(diffKW) > 0 {
		c := Change{
			Type:    "modify-" + section,
			Section: section,
			Entity:  ename,
			Attrs:   diffKW,
		}
		if len(oldKW) > 0 {
			c.OldValue = oldKW
		}
		if len(newKW) > 0 {
			c.NewValue = newKW
		}
		changes = append(changes, c)
	}

	// Sub-entities (fields, states, actions, etc.)
	oldSubs := extractSubEntities(old)
	newSubs := extractSubEntities(new)

	oldByKind := groupByKind(oldSubs)
	newByKind := groupByKind(newSubs)

	for kind, oldList := range oldByKind {
		newList, hasNew := newByKind[kind]
		if hasNew {
			changes = append(changes, diffSubEntityList(section, ename, kind, oldList, newList)...)
		} else {
			for _, n := range oldList {
				sname := entityNameOf(n)
				if sname != "" {
					changes = append(changes, Change{
						Type:    "remove-" + kind,
						Section: section,
						Entity:  ename,
						Field:   sname,
					})
				}
			}
		}
	}
	for kind, newList := range newByKind {
		if _, hasOld := oldByKind[kind]; !hasOld {
			for _, n := range newList {
				sname := entityNameOf(n)
				if sname != "" {
					changes = append(changes, Change{
						Type:    "add-" + kind,
						Section: section,
						Entity:  ename,
						Field:   sname,
						Attrs:   extractAttrs(n),
					})
				}
			}
		}
	}

	return changes
}

func diffSubEntityList(section, ename, kind string, old, new []*sNode) []Change {
	oldMap := make(map[string]*sNode)
	newMap := make(map[string]*sNode)
	allNames := make(map[string]bool)

	for _, n := range old {
		sname := entityNameOf(n)
		if sname != "" {
			oldMap[sname] = n
			allNames[sname] = true
		}
	}
	for _, n := range new {
		sname := entityNameOf(n)
		if sname != "" {
			newMap[sname] = n
			allNames[sname] = true
		}
	}

	var changes []Change
	sortedNames := make([]string, 0, len(allNames))
	for n := range allNames {
		sortedNames = append(sortedNames, n)
	}
	sort.Strings(sortedNames)

	for _, sname := range sortedNames {
		oldN, hasOld := oldMap[sname]
		newN, hasNew := newMap[sname]
		if hasOld && hasNew {
			oldKW := extractAttrs(oldN)
			newKW := extractAttrs(newN)
			diffKW := diffKeywordMaps(oldKW, newKW)
			if len(diffKW) > 0 {
				changes = append(changes, Change{
					Type:     "modify-" + kind,
					Section:  section,
					Entity:   ename,
					Field:    sname,
					Attrs:    diffKW,
					OldValue: oldKW,
					NewValue: newKW,
				})
			}
		} else if hasOld {
			changes = append(changes, Change{
				Type:    "remove-" + kind,
				Section: section,
				Entity:  ename,
				Field:   sname,
			})
		} else {
			changes = append(changes, Change{
				Type:    "add-" + kind,
				Section: section,
				Entity:  ename,
				Field:   sname,
				Attrs:   extractAttrs(newN),
			})
		}
	}

	return changes
}

// ---------------------------------------------------------------------------
// Sub-entity helpers
// ---------------------------------------------------------------------------

func removeSubEntity(section, ename string, n *sNode) []Change {
	var changes []Change
	for _, sub := range extractSubEntities(n) {
		kind := entityKindOf(sub)
		sname := entityNameOf(sub)
		if sname != "" {
			changes = append(changes, Change{
				Type:    "remove-" + kind,
				Section: section,
				Entity:  ename,
				Field:   sname,
			})
		}
	}
	return changes
}

func addSubEntity(section, ename string, n *sNode) []Change {
	var changes []Change
	for _, sub := range extractSubEntities(n) {
		kind := entityKindOf(sub)
		sname := entityNameOf(sub)
		if sname != "" {
			changes = append(changes, Change{
				Type:    "add-" + kind,
				Section: section,
				Entity:  ename,
				Field:   sname,
				Attrs:   extractAttrs(sub),
			})
		}
	}
	return changes
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// entityNameOf returns the name of an entity form, which is the second child.
func entityNameOf(n *sNode) string {
	if !n.IsList || len(n.Children) < 2 {
		return ""
	}
	return nodeStr(n.Children[1])
}

// entityKindOf returns the head of an entity form (e.g., "field", "state").
func entityKindOf(n *sNode) string {
	if !n.IsList || len(n.Children) == 0 {
		return "subentity"
	}
	return nodeStr(n.Children[0])
}

func subEntities(sec *sNode) []*sNode {
	if !sec.IsList || len(sec.Children) < 2 {
		return nil
	}
	var entities []*sNode
	for _, c := range sec.Children[1:] {
		if c.IsList && len(c.Children) >= 2 {
			entities = append(entities, c)
		}
	}
	return entities
}

func extractAttrs(n *sNode) map[string]interface{} {
	if !n.IsList || len(n.Children) < 3 {
		return nil
	}
	attrs := make(map[string]interface{})
	for i := 2; i < len(n.Children); i++ {
		child := n.Children[i]
		if child.AtomType == sTokKeyword && i+1 < len(n.Children) {
			valNode := n.Children[i+1]
			attrs[child.Atom] = parseAttrValue(valNode)
			i++
		}
	}
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}

func parseAttrValue(n *sNode) interface{} {
	if n.IsList {
		return nodeStr(n)
	}
	switch n.AtomType {
	case sTokInt:
		if v, err := strconv.ParseInt(n.Atom, 10, 64); err == nil {
			return v
		}
	case sTokFloat:
		if v, err := strconv.ParseFloat(n.Atom, 64); err == nil {
			return v
		}
	case sTokBool:
		return n.Atom == "true"
	case sTokString:
		return n.Atom
	}
	return n.Atom
}

func extractSubEntities(n *sNode) []*sNode {
	if !n.IsList || len(n.Children) < 3 {
		return nil
	}
	var subs []*sNode
	for i := 2; i < len(n.Children); i++ {
		child := n.Children[i]
		if !child.IsList || len(child.Children) < 2 {
			continue
		}
		head := nodeStr(child.Children[0])
		switch head {
		case "field", "constraint", "doc-constraint", "state", "action", "notification",
			"on-transition", "on-success", "on-failure":
			subs = append(subs, child)
		}
	}
	return subs
}

func groupByKind(nodes []*sNode) map[string][]*sNode {
	result := make(map[string][]*sNode)
	for _, n := range nodes {
		if !n.IsList || len(n.Children) == 0 {
			continue
		}
		kind := nodeStr(n.Children[0])
		switch kind {
		case "on-transition", "on-success", "on-failure":
			kind = "workflow-action"
		}
		result[kind] = append(result[kind], n)
	}
	return result
}

func diffKeywordMaps(old, new map[string]interface{}) map[string]interface{} {
	diff := make(map[string]interface{})
	for k, v := range old {
		if nv, ok := new[k]; !ok {
			diff[k] = nil
		} else if !kvEqual(v, nv) {
			diff[k] = nv
		}
	}
	for k, v := range new {
		if _, ok := old[k]; !ok {
			diff[k] = v
		}
	}
	if len(diff) == 0 {
		return nil
	}
	return diff
}

func kvEqual(a, b interface{}) bool {
	aStr, aOK := a.(string)
	bStr, bOK := b.(string)
	if aOK && bOK {
		return aStr == bStr
	}
	aInt, aOK := a.(int64)
	bInt, bOK := b.(int64)
	if aOK && bOK {
		return aInt == bInt
	}
	aFlt, aOK := a.(float64)
	bFlt, bOK := b.(float64)
	if aOK && bOK {
		return aFlt == bFlt
	}
	aBool, aOK := a.(bool)
	bBool, bOK := b.(bool)
	if aOK && bOK {
		return aBool == bBool
	}
	return a == b
}

// ---------------------------------------------------------------------------
// ReverseChanges
// ---------------------------------------------------------------------------

// ReverseChanges reverses a change list (for rollback).
func ReverseDiff(changes []Change) []Change {
	reversed := make([]Change, len(changes))
	for i, c := range changes {
		rc := Change{
			Section: c.Section,
			Entity:  c.Entity,
			Field:   c.Field,
			Attrs:   c.Attrs,
		}

		switch {
		case len(c.Type) > 4 && c.Type[:4] == "add-":
			rc.Type = "remove-" + c.Type[4:]
		case len(c.Type) > 7 && c.Type[:7] == "remove-":
			rc.Type = "add-" + c.Type[7:]
		default:
			rc.Type = c.Type
			rc.OldValue = c.NewValue
			rc.NewValue = c.OldValue
		}

		reversed[len(changes)-1-i] = rc
	}
	return reversed
}
