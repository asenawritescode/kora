package doctype

import (
	"strconv"
	"strings"
)

// MinVersionOK returns true if the running binary version >= the required version.
// Both are semver strings (e.g., "0.8.1" or "0.8.1-alpha").
// An empty requiredVersion or "dev" running version always returns true.
func MinVersionOK(runningVersion, requiredVersion string) bool {
	if requiredVersion == "" {
		return true
	}
	if runningVersion == "" || runningVersion == "dev" {
		return true // dev builds always satisfy the requirement
	}
	return semverGTE(runningVersion, requiredVersion)
}

// semverGTE returns true if a >= b using semver comparison (major.minor.patch).
func semverGTE(a, b string) bool {
	va := parseSemver(stripPrerelease(a))
	vb := parseSemver(stripPrerelease(b))
	for i := 0; i < 3; i++ {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return true
}

// stripPrerelease removes the pre-release suffix and build metadata from a version string.
func stripPrerelease(v string) string {
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		return v[:idx]
	}
	return v
}

// parseSemver parses "major.minor.patch" into [major, minor, patch].
// Missing parts are treated as 0.
func parseSemver(v string) [3]int {
	var result [3]int
	parts := strings.SplitN(v, ".", 3)
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err == nil {
			result[i] = n
		}
	}
	return result
}
