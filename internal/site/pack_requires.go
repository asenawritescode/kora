package site

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

// ErrPackRootRequiresMismatch marks a root pack.yaml whose requires differ from the
// marketplace manifest's requires. The manifest is what the Engine records and enforces.
var ErrPackRootRequiresMismatch = errors.New("pack.requires_mismatch")

// CheckPackRootRequires refuses a package whose root pack.yaml requires differ from
// the manifest requires, compared as sets of package and version. A package without
// a root pack.yaml has no file to disagree with.
func CheckPackRootRequires(manifest []contract.ManifestPackRef, files []PackFile) error {
	for _, f := range files {
		if strings.TrimSpace(f.Path) != packFileName {
			continue
		}
		raw, err := decodePackManifest([]byte(f.Content))
		if err != nil {
			return err
		}
		fromFile, fromManifest := requireKeys(raw.Requires), requireKeys(manifest)
		if !slices.Equal(fromFile, fromManifest) {
			return fmt.Errorf("%w: pack.yaml requires %v differ from the manifest requires %v", ErrPackRootRequiresMismatch, fromFile, fromManifest)
		}
	}
	return nil
}

// requireKeys is the sorted, de-duplicated package@version list of requires.
func requireKeys(refs []contract.ManifestPackRef) []string {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, ref.PackageID+"@"+ref.Version)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}
