package site

import (
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

// packScriptNameLimit is the width of _kora_script.name on every dialect.
const packScriptNameLimit = 140

// packScriptRule decides which script bindings a pack artifact may install. A
// pack script bound to <DocType> is accepted when the pack defines that DocType,
// or when a pack in its transitive requires closure defines it.
type packScriptRule struct {
	defined  map[string]map[string]bool // pack -> DocType names it defines
	requires map[string][]string        // pack -> packs it requires
}

func newPackScriptRule() *packScriptRule {
	return &packScriptRule{defined: map[string]map[string]bool{}, requires: map[string][]string{}}
}

// addPack records the DocTypes and requires of one pack of the artifact.
func (r *packScriptRule) addPack(name string, docTypes []*doctype.DocType, requires []contract.ManifestPackRef) {
	names := map[string]bool{}
	for _, dt := range docTypes {
		if dt != nil {
			names[dt.Name] = true
		}
	}
	r.defined[name] = names
	for _, ref := range requires {
		r.requires[name] = append(r.requires[name], ref.PackageID)
	}
}

// check rejects owned scripts whose row name overflows the column or whose
// binding is outside the rule. A binding to a DocType the script's own pack
// defines is marked ReplacesLegacy, so activation retires the unnamespaced row.
func (r *packScriptRule) check(scripts []*doctype.ScriptSnapshot) error {
	for _, ss := range scripts {
		if ss == nil || ss.OwnerPack == "" {
			continue
		}
		if len(ss.Name) > packScriptNameLimit {
			return fmt.Errorf("pack.script_name_too_long: script %q of pack %q is %d characters; the limit is %d", ss.Name, ss.OwnerPack, len(ss.Name), packScriptNameLimit)
		}
		if ss.DocType == "" {
			continue
		}
		if r.defined[ss.OwnerPack][ss.DocType] {
			ss.ReplacesLegacy = strings.ReplaceAll(ss.DocType, " ", "_") + "_" + ss.Event
			continue
		}
		found, external := r.requiredDefines(ss.OwnerPack, ss.DocType)
		switch {
		case found:
		case external:
			// A required pack outside the artifact may define it once installed.
			ss.PendingBinding = true
			ss.OwnerRequires = append([]string(nil), r.requires[ss.OwnerPack]...)
		default:
			return fmt.Errorf("pack.script_binding_undefined: scripts/%s/%s.js in pack %q binds DocType %q, which the pack neither defines nor requires", ss.DocType, ss.Event, ss.OwnerPack, ss.DocType)
		}
	}
	return nil
}

// requiredDefines walks the transitive requires closure of pack. found reports
// that a pack in the artifact defines docType; external reports that the closure
// reaches a pack outside the artifact, whose DocTypes are only known once installed.
func (r *packScriptRule) requiredDefines(pack, docType string) (found, external bool) {
	seen := map[string]bool{pack: true}
	queue := append([]string(nil), r.requires[pack]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true
		if _, inArtifact := r.defined[next]; !inArtifact {
			external = true
			continue
		}
		if r.defined[next][docType] {
			return true, false
		}
		queue = append(queue, r.requires[next]...)
	}
	return false, external
}

// packScriptRowName is the _kora_script name of a pack's document-event or flat script.
func packScriptRowName(pack, base string) string {
	return pack + "__" + base
}
