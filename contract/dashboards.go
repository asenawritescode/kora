package contract

// DashboardDefinition is one dashboard the caller may read, with the tenant's layout override applied.
// Tiles and charts the caller cannot read are absent.
//
//contract:wire
type DashboardDefinition struct {
	Name         string           `json:"name"`
	Label        string           `json:"label"`
	Description  string           `json:"description,omitempty"`
	DefaultRange string           `json:"default_range"`
	Tiles        []DashboardTile  `json:"tiles"`
	Charts       []DashboardChart `json:"charts"`
}

// DashboardTile shows one metric's value for the dashboard range.
//
//contract:wire
type DashboardTile struct {
	Metric  string `json:"metric"`
	Compare string `json:"compare,omitempty"`
}

// DashboardChart plots one to four metrics by time or by a dimension.
//
//contract:wire
type DashboardChart struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Kind    string   `json:"kind"`
	Metrics []string `json:"metrics"`
	By      string   `json:"by"`
	Grain   string   `json:"grain,omitempty"`
	Top     int      `json:"top,omitempty"`
	Compare string   `json:"compare,omitempty"`
	Drill   bool     `json:"drill,omitempty"`
}

// DashboardOverride is a tenant's layout of one dashboard. It replaces the previous override whole.
// Hidden and Order name tiles by metric and charts by id; a name the dashboard lacks is ignored.
// An empty DefaultRange keeps the pack's default.
//
//contract:wire
type DashboardOverride struct {
	Hidden       []string `json:"hidden"`
	Order        []string `json:"order"`
	DefaultRange string   `json:"default_range,omitempty"`
}
