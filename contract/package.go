package contract

// MarketplacePackageManifest is the immutable marketplace manifest shared by Cloud and
// Engine. Storage keys, review state, and timestamps belong to Cloud records.
//
//contract:wire:v2
type MarketplacePackageManifest struct {
	SchemaVersion int                 `json:"schema_version"`
	PackageID     string              `json:"package_id"`
	ReleaseID     string              `json:"release_id"`
	Version       string              `json:"version"`
	Kind          string              `json:"kind"`
	Public        ManifestPublic      `json:"public"`
	Requires      []ManifestPackRef   `json:"requires,omitempty"`
	Exports       ManifestExports     `json:"exports,omitempty"`
	Conflicts     []ManifestPackRef   `json:"conflicts,omitempty"`
	Composition   []ManifestComponent `json:"composition,omitempty"`
	Engine        ManifestEngine      `json:"engine"`
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
	SchemaVersion int               `json:"schema_version"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Requires      []ManifestPackRef `json:"requires,omitempty"`
	Exports       ManifestExports   `json:"exports"`
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
