package eventbus

import (
	"errors"
	"io/fs"
	"path/filepath"
)

// legacyWALHasFiles reports whether dir holds any regular file, searching
// tenant partitions too. A missing or unreadable directory counts as empty.
func legacyWALHasFiles(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fs.SkipDir
		}
		if entry.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}
