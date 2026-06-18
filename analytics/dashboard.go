package analytics

// DashboardConfig defines a dashboard layout with widgets referencing metrics.
type DashboardConfig struct {
	Name        string         `json:"name" yaml:"name"`
	Label       string         `json:"label" yaml:"label"`
	Description string         `json:"description" yaml:"description"`
	IsDefault   bool           `json:"is_default" yaml:"is_default"`
	Widgets     []WidgetConfig `json:"widgets" yaml:"widgets"`
}

// WidgetConfig defines a single chart or stat on a dashboard.
type WidgetConfig struct {
	Name      string `json:"name" yaml:"name"`
	Title     string `json:"title" yaml:"title"`
	Metric    string `json:"metric" yaml:"metric"`       // references a Metric.Name
	ChartType string `json:"chart_type" yaml:"chart_type"` // line, bar, pie, donut, stat, table
	Width     int    `json:"w" yaml:"w"`
	Height    int    `json:"h" yaml:"h"`
	X         int    `json:"x" yaml:"x"`
	Y         int    `json:"y" yaml:"y"`
}
