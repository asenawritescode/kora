package script

import (
	"encoding/json"
	"fmt"
)

// globalNamesExpr evaluates to the names of the global object's own properties.
const globalNamesExpr = `Object.getOwnPropertyNames(globalThis)`

// newGlobalNames returns the names in after that were not in before: the globals one library defined.
func newGlobalNames(before, after []string) []string {
	seen := make(map[string]bool, len(before))
	for _, name := range before {
		seen[name] = true
	}
	var added []string
	for _, name := range after {
		if !seen[name] {
			added = append(added, name)
		}
	}
	return added
}

// lockGlobalsSource returns source that makes each named global read-only and
// non-configurable, so a later library or script cannot redeclare or replace it.
// The scripts still read the values.
func lockGlobalsSource(names []string) (string, error) {
	data, err := json.Marshal(names)
	if err != nil {
		return "", fmt.Errorf("script: encode library globals: %w", err)
	}
	return fmt.Sprintf(`(function (names) {
		for (var i = 0; i < names.length; i++) {
			Object.defineProperty(globalThis, names[i], {writable: false, configurable: false});
		}
	})(%s);`, data), nil
}
