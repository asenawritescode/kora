package contract

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MarketplacePackageManifest is the immutable marketplace manifest shared by Cloud and
// Engine. Storage keys, review state, and timestamps belong to Cloud records.
//
//contract:wire:v2
type MarketplacePackageManifest struct {
	SchemaVersion int                      `json:"schema_version"`
	PackageID     string                   `json:"package_id"`
	ReleaseID     string                   `json:"release_id"`
	Version       string                   `json:"version"`
	Kind          string                   `json:"kind"`
	Public        ManifestPublic           `json:"public"`
	Requires      []ManifestPackRef        `json:"requires,omitempty"`
	Exports       ManifestExports          `json:"exports,omitempty"`
	Conflicts     []ManifestPackRef        `json:"conflicts,omitempty"`
	Composition   []ManifestComponent      `json:"composition,omitempty"`
	Modules       []ModuleDescriptor       `json:"modules,omitempty"`
	ScheduledJobs []ScheduledJobDefinition `json:"scheduled_jobs,omitempty"`
	Engine        ManifestEngine           `json:"engine"`
}

// DecodeMarketplacePackageManifest strictly decodes the immutable package contract.
func DecodeMarketplacePackageManifest(data []byte) (MarketplacePackageManifest, error) {
	var manifest MarketplacePackageManifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return MarketplacePackageManifest{}, fmt.Errorf("decode package manifest: %w", err)
	}
	if decoder.More() {
		return MarketplacePackageManifest{}, fmt.Errorf("decode package manifest: trailing JSON value")
	}
	if len(manifest.Modules) > 0 {
		if err := ValidateModuleSet(manifest.Modules, nil); err != nil {
			return MarketplacePackageManifest{}, err
		}
	}
	if err := ValidateScheduledJobModuleOwners(manifest.ScheduledJobs, manifest.Modules); err != nil {
		return MarketplacePackageManifest{}, err
	}
	if err := ValidateManifestPackRefs(manifest.Requires, manifest.Conflicts); err != nil {
		return MarketplacePackageManifest{}, err
	}
	return manifest, nil
}

func ValidateScheduledJobModuleOwners(jobs []ScheduledJobDefinition, modules []ModuleDescriptor) error {
	owners := make(map[ModuleID]bool, len(modules))
	for _, module := range modules {
		owners[module.ID] = true
	}
	for _, job := range jobs {
		if job.OwnerModuleID != "" && !owners[job.OwnerModuleID] {
			return fmt.Errorf("scheduled job %q names an undeclared owner module", job.Name)
		}
	}
	return nil
}

// ValidateManifestPackRefs validates pinned package requirements and declared
// package/release conflicts before a composition can reach activation.
func ValidateManifestPackRefs(requires, conflicts []ManifestPackRef) error {
	if len(requires) > 128 || len(conflicts) > 128 {
		return fmt.Errorf("package requirements or conflicts exceed the supported limit")
	}
	validRef := func(ref ManifestPackRef, requireTarget bool) bool {
		valid := func(value string) bool {
			if value == "" || len(value) > 160 || strings.TrimSpace(value) != value || strings.Contains(value, "..") || strings.ContainsAny(value, "/\\") {
				return false
			}
			for _, r := range value {
				if r < 0x21 || r > 0x7e {
					return false
				}
			}
			return true
		}
		if ref.PackageID != "" && !valid(ref.PackageID) || ref.ReleaseID != "" && !valid(ref.ReleaseID) || ref.Version != "" && !valid(ref.Version) {
			return false
		}
		if requireTarget {
			return ref.PackageID != "" && (ref.ReleaseID != "" || ref.Version != "")
		}
		return ref.PackageID != "" || ref.ReleaseID != ""
	}
	seen := map[ManifestPackRef]bool{}
	for _, ref := range requires {
		if !validRef(ref, true) || seen[ref] {
			return fmt.Errorf("package requirement reference is invalid or duplicated")
		}
		seen[ref] = true
	}
	clear(seen)
	for _, ref := range conflicts {
		if !validRef(ref, false) || seen[ref] {
			return fmt.Errorf("package conflict reference is invalid or duplicated")
		}
		seen[ref] = true
	}
	return nil
}

// ScheduledJobDefinition is an immutable tenant job supplied by a package.
// Type is application-owned; Engine validates and dispatches it generically.
//
//contract:wire:v2
type ScheduledJobDefinition struct {
	Name              string         `json:"name"`
	OwnerModuleID     ModuleID       `json:"owner_module_id,omitempty"`
	Type              string         `json:"type"`
	Schedule          string         `json:"schedule"`
	Scope             *BusinessScope `json:"scope,omitempty"`
	RequiredResource  string         `json:"required_resource,omitempty"`
	RequiredOperation string         `json:"required_operation,omitempty"`
	//contract:json
	Config json.RawMessage `json:"config,omitempty"`
}

// ValidateScheduledJobDefinitions checks the scheduler contract shared by
// Cloud and Engine. Job execution remains application-owned.
func ValidateScheduledJobDefinitions(jobs []ScheduledJobDefinition) error {
	if len(jobs) > 64 {
		return fmt.Errorf("scheduled_jobs cannot contain more than 64 definitions")
	}
	names := make(map[string]bool, len(jobs))
	configBytes := 0
	for _, job := range jobs {
		name := strings.TrimSpace(job.Name)
		jobType := strings.TrimSpace(job.Type)
		if name == "" || name != job.Name || len(name) > 128 || jobType == "" || jobType != job.Type || len(jobType) > 128 || strings.TrimSpace(job.Schedule) == "" || len(job.Schedule) > 128 {
			return fmt.Errorf("scheduled jobs require a name, type, and supported-length schedule")
		}
		if names[name] {
			return fmt.Errorf("scheduled job name %q is duplicated", name)
		}
		if job.OwnerModuleID != "" && !moduleIDPattern.MatchString(string(job.OwnerModuleID)) {
			return fmt.Errorf("scheduled job %q has invalid owner module", name)
		}
		names[name] = true
		if err := ValidateCronSchedule(job.Schedule); err != nil {
			return fmt.Errorf("scheduled job %q: %w", name, err)
		}
		if job.Scope != nil {
			if err := job.Scope.Validate(); err != nil {
				return fmt.Errorf("scheduled job %q scope: %w", name, err)
			}
			if job.Scope.Kind == ScopeKindLegalEntity && (job.RequiredResource == "" || job.RequiredOperation == "") {
				return fmt.Errorf("scheduled job %q with legal-entity scope requires an explicit resource and operation grant", name)
			}
		}
		if len(job.Config) == 0 {
			continue
		}
		trimmed := strings.TrimSpace(string(job.Config))
		if !json.Valid(job.Config) || trimmed == "" || trimmed[0] != '{' {
			return fmt.Errorf("scheduled job %q config must be a JSON object", name)
		}
		configBytes += len(job.Config)
	}
	if configBytes > 64<<10 {
		return fmt.Errorf("scheduled job configuration exceeds 64 KiB")
	}
	return nil
}

// ValidateCronSchedule checks the supported five-field cron subset.
func ValidateCronSchedule(expression string) error {
	parts := strings.Fields(expression)
	if len(parts) != 5 {
		return fmt.Errorf("schedule must contain five cron fields")
	}
	for index, max := range []int{59, 23} {
		value := parts[index]
		if value == "*" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(value, "%d", &n); err != nil || n < 0 || n > max || fmt.Sprint(n) != value {
			return fmt.Errorf("cron field %d must be * or an integer from 0 to %d", index+1, max)
		}
	}
	for _, value := range parts[2:] {
		if value != "*" {
			return fmt.Errorf("day, month, and weekday cron fields must be *")
		}
	}
	return nil
}

//contract:wire:v2
type ManifestPublic struct {
	Slug         string                     `json:"slug"`
	Name         string                     `json:"name"`
	Summary      string                     `json:"summary"`
	Category     string                     `json:"category,omitempty"`
	Audiences    []string                   `json:"audiences,omitempty"`
	Outcomes     []string                   `json:"outcomes,omitempty"`
	Requirements []string                   `json:"requirements,omitempty"`
	DetailPage   *TemplateDetailPageContent `json:"detail_page,omitempty"`
	Price        *ManifestPrice             `json:"price,omitempty"`
}

// TemplateDetailPageContent carries customer-facing template detail labels.
//
//contract:wire:v2
type TemplateDetailPageContent struct {
	BreadcrumbLabel        string `json:"breadcrumb_label"`
	TemplatesLink          string `json:"templates_link"`
	PrimaryAction          string `json:"primary_action"`
	PrimaryReassurance     string `json:"primary_reassurance"`
	PreparationHeading     string `json:"preparation_heading"`
	OutcomesHeading        string `json:"outcomes_heading"`
	CompositionHeading     string `json:"composition_heading"`
	PartLabel              string `json:"part_label"`
	PartsLabel             string `json:"parts_label"`
	IncludedLabel          string `json:"included_label"`
	OptionalLabel          string `json:"optional_label"`
	OptionalContinueAction string `json:"optional_continue_action"`
	UnavailableHeading     string `json:"unavailable_heading"`
	UnavailableDescription string `json:"unavailable_description"`
}

//contract:wire:v2
type ManifestPrice struct {
	Mode        string `json:"mode"`
	PlanCode    string `json:"plan_code,omitempty"`
	Currency    string `json:"currency,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	Period      string `json:"period,omitempty"`
}

//contract:wire:v2
type ManifestEngine struct {
	MinVersion string `json:"min_version"`
}

//contract:wire:v2
type ManifestPackRef struct {
	PackageID string `json:"package_id"`
	ReleaseID string `json:"release_id,omitempty"`
	Version   string `json:"version,omitempty"`
}

//contract:wire:v2
type ManifestExports struct {
	Doctypes     []string `json:"doctypes,omitempty"`
	Commands     []string `json:"commands,omitempty"`
	Events       []string `json:"events,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

//contract:wire:v2
type ManifestComponent struct {
	ReleaseID string `json:"release_id"`
	Selection string `json:"selection"`
	Unlocks   string `json:"unlocks,omitempty"`
}

// PackManifest is the Engine-validation projection of MarketplacePackageManifest.
//
//contract:wire:v2
type PackManifest struct {
	SchemaVersion int                      `json:"schema_version"`
	Name          string                   `json:"name"`
	Version       string                   `json:"version"`
	Requires      []ManifestPackRef        `json:"requires,omitempty"`
	Conflicts     []ManifestPackRef        `json:"conflicts,omitempty"`
	Exports       ManifestExports          `json:"exports"`
	ScheduledJobs []ScheduledJobDefinition `json:"scheduled_jobs,omitempty"`
	Modules       []ModuleDescriptor       `json:"modules,omitempty"`
}

// MarketplacePackManifest projects the canonical marketplace declaration into
// the Engine package validation contract. Keep this as the sole projection so
// validation and activation cannot drift as PackManifest evolves.
func MarketplacePackManifest(m MarketplacePackageManifest) PackManifest {
	return PackManifest{SchemaVersion: m.SchemaVersion, Name: m.PackageID, Version: m.Version, Requires: m.Requires, Conflicts: m.Conflicts, Exports: m.Exports, ScheduledJobs: m.ScheduledJobs, Modules: m.Modules}
}

//contract:wire:v2
type PackFile struct {
	Path string `json:"path"`
	//contract:maxBytes=262144
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
}

//contract:wire:v2
type PackFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

//contract:wire:v2
type PackValidation struct {
	Valid    bool          `json:"valid"`
	Findings []PackFinding `json:"findings"`
}
