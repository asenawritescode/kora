// Package envcompat lets the process accept ZANA_* environment variables while
// every existing reader keeps looking at KORA_*.
//
// ZANA_<S> is the preferred spelling of KORA_<S>. Promote copies each ZANA_<S>
// into KORA_<S> in the process environment before configuration is read, so no
// call site changes. The mapping is by prefix only: KORA_CLOUD_KORA_RELOAD_URL
// pairs with ZANA_CLOUD_KORA_RELOAD_URL.
//
// An empty value counts as unset, matching how configuration readers treat it.
// If both spellings are set to different values, ZANA_* wins.
//
// Nothing in this package ever logs or returns a variable's value.
package envcompat

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
)

const (
	preferredPrefix = "ZANA_"
	legacyPrefix    = "KORA_"
)

// Report names the variables that need an operator's attention. It holds
// names only, never values.
type Report struct {
	// Deprecated lists KORA_* variables in effect with no ZANA_* counterpart.
	Deprecated []string
	// Overridden lists ZANA_* variables whose value replaced a different
	// value of the KORA_* twin.
	Overridden []string
}

// Apply promotes ZANA_* variables in the real process environment and logs the
// resulting report to log. Call it first thing in main, before any
// configuration is read.
func Apply(log *slog.Logger) (Report, error) {
	report, err := Promote(os.Environ(), os.Setenv)
	report.Log(log)
	return report, err
}

// Promote copies ZANA_<S> to KORA_<S> through setenv for every ZANA_* entry in
// environ with a non-empty value, and reports deprecated and overridden names.
// Both slices in the Report are sorted.
func Promote(environ []string, setenv func(key, value string) error) (Report, error) {
	vars := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		vars[key] = value
	}

	var report Report
	var errs []error
	for key, value := range vars {
		suffix, ok := strings.CutPrefix(key, preferredPrefix)
		if !ok || suffix == "" || value == "" {
			continue
		}
		legacy := legacyPrefix + suffix
		old := vars[legacy]
		if old == value {
			continue
		}
		if old != "" {
			report.Overridden = append(report.Overridden, key)
		}
		if err := setenv(legacy, value); err != nil {
			errs = append(errs, fmt.Errorf("promote %s to %s: %w", key, legacy, err))
		}
	}
	for key, value := range vars {
		suffix, ok := strings.CutPrefix(key, legacyPrefix)
		if !ok || suffix == "" || value == "" {
			continue
		}
		if vars[preferredPrefix+suffix] == "" {
			report.Deprecated = append(report.Deprecated, key)
		}
	}
	sort.Strings(report.Deprecated)
	sort.Strings(report.Overridden)
	return report, errors.Join(errs...)
}

// Log writes at most one line per kind of finding, naming variables only.
func (r Report) Log(log *slog.Logger) {
	if log == nil {
		return
	}
	if len(r.Overridden) > 0 {
		log.Warn("both ZANA_* and KORA_* are set to different values; ZANA_* wins",
			"variables", strings.Join(r.Overridden, ","))
	}
	if len(r.Deprecated) > 0 {
		log.Warn("deprecated KORA_* environment variables in use; rename them to ZANA_* (KORA_* support will be removed in the release after next)",
			"variables", strings.Join(r.Deprecated, ","))
	}
}
