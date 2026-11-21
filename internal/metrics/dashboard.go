package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/asenawritescode/kora/internal/doctype"
	goyaml "github.com/goccy/go-yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// Dashboard lays out metrics by name: tiles for single values and charts over time or a dimension.
// It never names SQL, models or fields. One dashboard is one YAML document in a pack's dashboards/ directory.
type Dashboard struct {
	Name  string `yaml:"name" json:"name"`
	Label string `yaml:"label" json:"label"`
	// Pack is the display label of the pack that ships the dashboard, set from its pack.yaml when the pack loads.
	Pack         string  `yaml:"-" json:"pack,omitempty"`
	Description  string  `yaml:"description,omitempty" json:"description,omitempty"`
	DefaultRange string  `yaml:"default_range" json:"default_range"`
	Tiles        []Tile  `yaml:"tiles,omitempty" json:"tiles,omitempty"`
	Charts       []Chart `yaml:"charts,omitempty" json:"charts,omitempty"`
}

// Tile shows the value of one metric for the dashboard range.
type Tile struct {
	Metric  string `yaml:"metric" json:"metric"`
	Compare string `yaml:"compare,omitempty" json:"compare,omitempty"`
}

// Chart plots one to four metrics by time or by a dimension the metrics declare.
type Chart struct {
	ID      string   `yaml:"id" json:"id"`
	Title   string   `yaml:"title" json:"title"`
	Kind    string   `yaml:"kind" json:"kind"`
	Metrics []string `yaml:"metrics" json:"metrics"`
	By      string   `yaml:"by" json:"by"`
	Grain   string   `yaml:"grain,omitempty" json:"grain,omitempty"`
	Top     int      `yaml:"top,omitempty" json:"top,omitempty"`
	Compare string   `yaml:"compare,omitempty" json:"compare,omitempty"`
	Drill   bool     `yaml:"drill,omitempty" json:"drill,omitempty"`
}

// Dashboard vocabularies.
const (
	byTime       = "time"
	maxChartRefs = 4
)

var (
	dashboardKeys = yamlKeys(Dashboard{})
	tileKeys      = yamlKeys(Tile{})
	chartKeys     = yamlKeys(Chart{})
	rangeNames    = presetNames()
	compareNames  = []string{"", "none", "previous_period", "week_before"}
	chartKinds    = []string{"bar", "line", "table"}
	grainNames    = []string{"day", "week", "month"}
)

// ValidDashboardRange reports whether name is a range a dashboard may default to.
func ValidDashboardRange(name string) bool {
	return slices.Contains(rangeNames, name)
}

// ParseDashboardDirectory reads the dashboard documents of a pack's dashboards/ directory. A missing directory has none.
func ParseDashboardDirectory(dir string) ([]Dashboard, error) {
	return readYAMLDir(dir, ParseDashboardYAML)
}

// ParsePackDashboards reads a pack directory's dashboards and labels each with the pack's display label from its
// pack.yaml. A pack without a label leaves Pack empty.
func ParsePackDashboards(packDir string) ([]Dashboard, error) {
	dashboards, err := ParseDashboardDirectory(filepath.Join(packDir, "dashboards"))
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(packDir, "pack.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return dashboards, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pack.yaml: %w", err)
	}
	label, err := PackLabel(body)
	if err != nil {
		return nil, err
	}
	for i := range dashboards {
		dashboards[i].Pack = label
	}
	return dashboards, nil
}

// PackLabel reads the optional display label of a pack from its pack.yaml bytes.
func PackLabel(body []byte) (string, error) {
	var head struct {
		Label string `yaml:"label"`
	}
	if err := goyaml.Unmarshal(body, &head); err != nil {
		return "", fmt.Errorf("pack.yaml: %w", err)
	}
	return head.Label, nil
}

// ParseDashboardYAML decodes one dashboard document; source names it in errors.
func ParseDashboardYAML(body []byte, source string) (Dashboard, error) {
	var d Dashboard
	if err := checkKeys(body, dashboardKeys, dashboardNestedProblems); err != nil {
		return d, fmt.Errorf("%s: %w", source, err)
	}
	if err := goyaml.UnmarshalWithOptions(body, &d, goyaml.DisallowUnknownField()); err != nil {
		return d, fmt.Errorf("%s: yaml: %w", source, err)
	}
	return d, nil
}

// dashboardNestedProblems checks the keys of each tile and chart.
func dashboardNestedProblems(key string, v *yamlv3.Node) []string {
	var known map[string]bool
	switch {
	case key == "tiles" && v.Kind == yamlv3.SequenceNode:
		known = tileKeys
	case key == "charts" && v.Kind == yamlv3.SequenceNode:
		known = chartKeys
	default:
		return nil
	}
	var problems []string
	for _, item := range v.Content {
		if item.Kind == yamlv3.MappingNode {
			problems = append(problems, unknownKeys(item, known)...)
		}
	}
	return problems
}

// ValidateDashboards checks dashboards against the metrics and record types of the same import and
// returns every problem at once.
func ValidateDashboards(dashboards []Dashboard, defs []Metric, doctypes []*doctype.DocType) error {
	c, errs := newCatalog(defs, doctypes)
	names := map[string]bool{}
	for _, d := range dashboards {
		if names[d.Name] {
			errs = append(errs, fmt.Errorf("dashboard %q is declared more than once", d.Name))
			continue
		}
		names[d.Name] = true
		if err := d.validate(c); err != nil {
			errs = append(errs, fmt.Errorf("dashboard %q: %w", d.Name, err))
		}
	}
	return errors.Join(errs...)
}

// validate checks the dashboard header, its tiles and its charts. Chart ids are unique within the dashboard.
func (d Dashboard) validate(c catalog) error {
	switch {
	case !nameRE.MatchString(d.Name):
		return errors.New("name must be lowercase snake_case")
	case strings.TrimSpace(d.Label) == "":
		return errors.New("label is required")
	case !slices.Contains(rangeNames, d.DefaultRange):
		return fmt.Errorf("default_range %q is not one of %q", d.DefaultRange, rangeNames)
	}
	for _, t := range d.Tiles {
		if err := t.validate(c); err != nil {
			return fmt.Errorf("tile %q: %w", t.Metric, err)
		}
	}
	ids := map[string]bool{}
	for _, ch := range d.Charts {
		if ids[ch.ID] {
			return fmt.Errorf("chart %q is declared more than once", ch.ID)
		}
		ids[ch.ID] = true
		if err := ch.validate(c); err != nil {
			return fmt.Errorf("chart %q: %w", ch.ID, err)
		}
	}
	return nil
}

// validate checks that the tile names a known metric and that its compare mode is known.
func (t Tile) validate(c catalog) error {
	if _, ok := c.metrics[t.Metric]; !ok {
		return fmt.Errorf("unknown metric %q", t.Metric)
	}
	if !slices.Contains(compareNames, t.Compare) {
		return fmt.Errorf("compare %q is not one of %q", t.Compare, compareNames)
	}
	return nil
}

// validate checks a chart's kind, metrics, compare mode and its by: time or dimension axis.
func (ch Chart) validate(c catalog) error {
	switch {
	case strings.TrimSpace(ch.ID) == "":
		return errors.New("id is required")
	case strings.TrimSpace(ch.Title) == "":
		return errors.New("title is required")
	case !slices.Contains(chartKinds, ch.Kind):
		return fmt.Errorf("kind %q is not one of %q", ch.Kind, chartKinds)
	case len(ch.Metrics) == 0 || len(ch.Metrics) > maxChartRefs:
		return fmt.Errorf("a chart takes 1 to %d metrics, got %d", maxChartRefs, len(ch.Metrics))
	case !slices.Contains(compareNames, ch.Compare):
		return fmt.Errorf("compare %q is not one of %q", ch.Compare, compareNames)
	case ch.Top < 0:
		return fmt.Errorf("top %d must be positive", ch.Top)
	case ch.By == "":
		return errors.New("by is required: time or a dimension")
	}
	if err := ch.checkAxis(); err != nil {
		return err
	}
	for _, name := range ch.Metrics {
		if _, ok := c.metrics[name]; !ok {
			return fmt.Errorf("unknown metric %q", name)
		}
		if ch.By != byTime {
			if err := c.checkDimension(name, ch.By); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkAxis checks the parts of a chart that depend on whether it plots time or a dimension.
func (ch Chart) checkAxis() error {
	if ch.By == byTime {
		if ch.Top != 0 {
			return errors.New("top needs a dimension, not by: time")
		}
		if !slices.Contains(grainNames, ch.Grain) {
			return fmt.Errorf("grain %q must be day, week or month when by is time", ch.Grain)
		}
		return nil
	}
	if ch.Grain != "" {
		return errors.New("grain applies to by: time only")
	}
	return nil
}

// checkDimension checks that every fact model a metric reads declares column as a dimension.
func (c catalog) checkDimension(name, column string) error {
	models, err := c.leafModels(name, map[string]bool{})
	if err != nil {
		return err
	}
	for _, model := range models {
		if dt := c.models[model]; dt == nil || dt.Facts == nil || !isDimension(dt.Facts, column) {
			return fmt.Errorf("%q is not a dimension of metric %q", column, name)
		}
	}
	return nil
}

// leafModels lists the fact models a metric reads, through its ratio and derived references.
// seen holds the metrics on the current path, so a cycle is reported rather than followed.
func (c catalog) leafModels(name string, seen map[string]bool) ([]string, error) {
	m, ok := c.metrics[name]
	if !ok {
		return nil, fmt.Errorf("unknown metric %q", name)
	}
	if seen[name] {
		return nil, fmt.Errorf("metric %q is part of a cycle", name)
	}
	if m.Model != "" {
		return []string{m.Model}, nil
	}
	seen[name] = true
	defer delete(seen, name)
	var models []string
	for _, ref := range m.refs() {
		refModels, err := c.leafModels(ref, seen)
		if err != nil {
			return nil, err
		}
		models = append(models, refModels...)
	}
	return models, nil
}
