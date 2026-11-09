package contract

import (
	"fmt"
	"strconv"
	"strings"
)

// EngineVersionAtLeast reports running >= required using semver 2.0.0
// section 11 precedence. An optional leading "v" and "+build" metadata are
// accepted; build metadata is ignored in comparison.
func EngineVersionAtLeast(running, required string) (bool, error) {
	got, err := parseEngineVersion(running)
	if err != nil {
		return false, err
	}
	want, err := parseEngineVersion(required)
	if err != nil {
		return false, err
	}
	return got.compare(want) >= 0, nil
}

type engineVersion struct {
	core [3]uint64
	pre  []string
}

func parseEngineVersion(value string) (engineVersion, error) {
	var v engineVersion
	bad := func() (engineVersion, error) {
		return v, fmt.Errorf("version %q must use semver MAJOR.MINOR.PATCH[-prerelease][+build]", value)
	}
	rest := strings.TrimPrefix(strings.TrimSpace(value), "v")
	if i := strings.Index(rest, "+"); i >= 0 {
		if !validIdentifiers(rest[i+1:], false) {
			return bad()
		}
		rest = rest[:i]
	}
	if i := strings.Index(rest, "-"); i >= 0 {
		if !validIdentifiers(rest[i+1:], true) {
			return bad()
		}
		v.pre = strings.Split(rest[i+1:], ".")
		rest = rest[:i]
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return bad()
	}
	for i, part := range parts {
		if !semverIdentifier(part).numeric() || (len(part) > 1 && part[0] == '0') {
			return bad()
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return bad()
		}
		v.core[i] = n
	}
	return v, nil
}

// semverIdentifier is one dot-separated piece of a semver version or prerelease.
type semverIdentifier string

// numeric reports whether the identifier is a non-empty run of ASCII digits.
func (s semverIdentifier) numeric() bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// validIdentifiers checks dot-separated [0-9A-Za-z-] identifiers; prerelease
// numeric identifiers must not have leading zeros.
func validIdentifiers(s string, prerelease bool) bool {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for _, c := range id {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
				return false
			}
		}
		if prerelease && semverIdentifier(id).numeric() && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// compare orders two versions by semver 2.0.0 section 11 precedence and returns -1, 0 or 1.
func (a engineVersion) compare(b engineVersion) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			return cmpUint(a.core[i], b.core[i])
		}
	}
	return comparePrerelease(a.pre, b.pre)
}

// comparePrerelease orders prerelease identifier lists. A version without a prerelease ranks above
// one with a prerelease, and identifiers are compared left to right.
func comparePrerelease(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return compareIdentifiers(semverIdentifier(a[i]), semverIdentifier(b[i]))
		}
	}
	return cmpInt(len(a), len(b))
}

// compareIdentifiers orders two different prerelease identifiers: numeric ones sort numerically and
// below alphanumeric ones.
func compareIdentifiers(x, y semverIdentifier) int {
	switch xn, yn := x.numeric(), y.numeric(); {
	case xn && yn:
		// Compare numerically without overflow: shorter is smaller.
		if len(x) != len(y) {
			return cmpInt(len(x), len(y))
		}
	case xn:
		return -1
	case yn:
		return 1
	}
	return strings.Compare(string(x), string(y))
}

func cmpUint(a, b uint64) int {
	if a < b {
		return -1
	}
	return 1
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
