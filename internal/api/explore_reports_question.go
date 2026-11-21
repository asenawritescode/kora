package api

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// Broken kinds: the piece of a question a configuration change removed, as contract.SavedReportBroken.Kind.
const (
	brokenMissingRecordType = "missing_record_type"
	brokenMissingField      = "missing_field"
	brokenMissingMetric     = "missing_metric"
	brokenInvalidQuestion   = "invalid_question"
)

// questionError is a saved question that no longer checks. Broken names the missing piece for the UI; the
// message keeps the text for logs and for the refusal of a save.
type questionError struct {
	broken contract.SavedReportBroken
	msg    string
}

func (e *questionError) Error() string { return e.msg }

// missingPiece returns a questionError that names the piece b, with the message from format.
func missingPiece(b contract.SavedReportBroken, format string, args ...any) error {
	return &questionError{broken: b, msg: fmt.Sprintf(format, args...)}
}

// brokenPiece is the broken state of a question that lost kind's piece: a record type, or a field or measure
// of it. The label falls back to the key.
func brokenPiece(kind, recordType, field string) contract.SavedReportBroken {
	label := field
	if label == "" {
		label = recordType
	}
	return contract.SavedReportBroken{Kind: kind, RecordType: recordType, Field: field, Label: label}
}

// brokenOf returns the broken state an error from checkSavedQuestion stands for. An error without a named
// piece is an invalid question, labelled with its message.
func brokenOf(err error) *contract.SavedReportBroken {
	var qe *questionError
	if errors.As(err, &qe) {
		b := qe.broken
		return &b
	}
	return &contract.SavedReportBroken{Kind: brokenInvalidQuestion, Label: err.Error()}
}

// checkSavedQuestion checks a saved question against the catalog and returns the record types it reads.
// A record question names an offered measure of a record type, with "by" dimensions the type has and filters
// the fact model accepts. A metric question names a defined metric whose record types have every "by"
// dimension and filter field it uses.
func checkSavedQuestion(cat metrics.Catalog, join map[string]bool, q contract.SavedQuestion) ([]string, error) {
	forms := 0
	for _, set := range []bool{q.RecordType != "", q.Metric != "", q.AdHoc != nil} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return nil, errors.New("a report names one record type, one metric or one ad-hoc measure")
	}
	if err := checkQuestionSettings(q); err != nil {
		return nil, err
	}
	filters, err := filtersFromContract(q.Filters)
	if err != nil {
		return nil, err
	}
	if q.AdHoc != nil {
		return checkAdHocQuestion(cat, join, q)
	}
	if q.Metric != "" {
		return checkMetricQuestion(cat, join, q, filters)
	}
	return checkRecordQuestion(cat, join, q, filters)
}

// checkAdHocQuestion checks a question of one unsaved measure: the measure is checked the way a query's unsaved
// measure is, its record type must offer the "by" dimensions, and the paths it groups by are read as for a record question.
func checkAdHocQuestion(cat metrics.Catalog, join map[string]bool, q contract.SavedQuestion) ([]string, error) {
	m := *q.AdHoc
	rec, ok := cat.ExploreRecord(m.RecordType, join)
	if !ok {
		return nil, fmt.Errorf("record type %q is not in the explore catalog", m.RecordType)
	}
	filters, err := filtersFromContract(m.Filters)
	if err != nil {
		return nil, err
	}
	if err := checkBy(rec, q.By); err != nil {
		return nil, err
	}
	if _, _, err := cat.WithAdHoc([]metrics.AdHoc{{Model: m.RecordType, Agg: m.Agg, Field: m.Field, Filters: filters}}); err != nil {
		return nil, err
	}
	return withHops(cat, m.RecordType, questionPaths(q), []string{m.RecordType}), nil
}

// checkPresetPeriod checks the named period of a question: a preset takes no From or To; custom keeps them.
func checkPresetPeriod(q contract.SavedQuestion) error {
	if q.Period == "" {
		return nil
	}
	preset, err := metrics.ParsePreset(q.Period)
	if err != nil {
		return err
	}
	if preset != metrics.PresetCustom && (q.From != "" || q.To != "") {
		return fmt.Errorf("period %q takes no from or to", preset)
	}
	return nil
}

// questionPaths is every dimension path and filter field a question names.
func questionPaths(q contract.SavedQuestion) []string {
	paths := append([]string{}, q.By...)
	for _, f := range q.Filters {
		paths = append(paths, f.Field)
	}
	return paths
}

// withHops adds to types every record type the paths of a question read through, and returns the result
// sorted and without repeats, so the caller can check read access to all of it.
func withHops(cat metrics.Catalog, model string, paths []string, types []string) []string {
	for _, path := range paths {
		types = append(types, linkHops(cat, model, path)...)
	}
	slices.Sort(types)
	return slices.Compact(types)
}

// linkHops returns the record types one path reads through: the target of each Link it follows, short of its
// last field.
func linkHops(cat metrics.Catalog, model, path string) []string {
	src, segments := pathStart(cat, cat.Models[model], strings.Split(path, "."))
	var hops []string
	for _, seg := range segments[:max(len(segments)-1, 0)] {
		fld := linkField(src, seg)
		if fld == nil {
			break
		}
		src = cat.Models[fld.Options]
		if src == nil {
			break
		}
		hops = append(hops, src.Name)
	}
	return hops
}

// pathStart returns the record type a path's fields are read from. A path that starts with the lines field
// reads from the child table it names.
func pathStart(cat metrics.Catalog, src *doctype.DocType, segments []string) (*doctype.DocType, []string) {
	if src == nil || src.Facts == nil || len(segments) < 2 || segments[0] != src.Facts.Lines {
		return src, segments
	}
	if lf := src.GetField(src.Facts.Lines); lf != nil {
		src = cat.Models[lf.Options]
	}
	return src, segments[1:]
}

// linkField returns the Link field seg of src, or nil when src has no such Link.
func linkField(src *doctype.DocType, seg string) *doctype.Field {
	if src == nil {
		return nil
	}
	fld := src.GetField(seg)
	if fld == nil || fld.Fieldtype != "Link" {
		return nil
	}
	return fld
}

// checkQuestionSettings checks the grain, comparison and period a question carries.
func checkQuestionSettings(q contract.SavedQuestion) error {
	if q.Grain != "" && !slices.Contains([]string{string(metrics.GrainDay), string(metrics.GrainWeek), string(metrics.GrainMonth), string(metrics.GrainRange)}, q.Grain) {
		return fmt.Errorf("grain %q is not day, week, month or range", q.Grain)
	}
	if q.Compare != "" && q.Compare != "none" && q.Compare != string(metrics.ComparePreviousPeriod) && q.Compare != string(metrics.CompareWeekBefore) {
		return fmt.Errorf("compare %q must be none, previous_period or week_before", q.Compare)
	}
	if q.Top < 0 {
		return errors.New("top cannot be negative")
	}
	if err := checkPresetPeriod(q); err != nil {
		return err
	}
	if q.From != "" || q.To != "" {
		if _, _, err := parseMetricRange(q.From, q.To); err != nil {
			return err
		}
	}
	return nil
}

// checkRecordQuestion checks a question of one record type's measure. The measure's field and filters are
// checked the way an unsaved measure of a query is.
func checkRecordQuestion(cat metrics.Catalog, join map[string]bool, q contract.SavedQuestion, filters []metrics.Filter) ([]string, error) {
	rec, ok := cat.ExploreRecord(q.RecordType, join)
	if !ok {
		return nil, missingPiece(brokenPiece(brokenMissingRecordType, q.RecordType, ""), "record type %q is not in the explore catalog", q.RecordType)
	}
	key := q.Measure
	if key == "" {
		key = "count"
	}
	measure, ok := exploreMeasure(rec, key)
	if !ok {
		return nil, missingPiece(brokenPiece(brokenMissingField, q.RecordType, key), "measure %q is not offered by %q", key, q.RecordType)
	}
	if err := checkBy(rec, q.By); err != nil {
		return nil, err
	}
	if _, _, err := cat.WithAdHoc([]metrics.AdHoc{{Model: q.RecordType, Agg: measure.Agg, Field: measure.Field, Filters: filters}}); err != nil {
		return nil, missingPiece(brokenPiece(brokenMissingField, q.RecordType, measure.Field), "%s", err)
	}
	return withHops(cat, q.RecordType, questionPaths(q), []string{q.RecordType}), nil
}

// checkMetricQuestion checks a question of a defined metric: its record types must offer the "by"
// dimensions and the filter fields.
func checkMetricQuestion(cat metrics.Catalog, join map[string]bool, q contract.SavedQuestion, filters []metrics.Filter) ([]string, error) {
	if _, ok := cat.Metrics[q.Metric]; !ok {
		return nil, missingPiece(brokenPiece(brokenMissingMetric, "", q.Metric), "metric %q is not defined", q.Metric)
	}
	if q.Measure != "" {
		return nil, errors.New("a metric question takes no measure")
	}
	ms, err := cat.Closure([]string{q.Metric})
	if err != nil {
		return nil, err
	}
	models := metrics.Models(ms)
	types := slices.Clone(models)
	for _, model := range models {
		rec, ok := cat.ExploreRecord(model, join)
		if !ok {
			return nil, missingPiece(brokenPiece(brokenMissingRecordType, model, ""), "record type %q of metric %q is not in the explore catalog", model, q.Metric)
		}
		if err := checkBy(rec, q.By); err != nil {
			return nil, err
		}
		for _, f := range filters {
			if !offersDimension(rec, f.Field) {
				return nil, missingPiece(brokenPiece(brokenMissingField, model, f.Field), "%q is not a dimension of %q", f.Field, model)
			}
		}
		types = withHops(cat, model, questionPaths(q), types)
	}
	slices.Sort(types)
	return slices.Compact(types), nil
}

// exploreMeasure returns the measure of rec with the catalog key.
func exploreMeasure(rec contract.ExploreRecordType, key string) (contract.ExploreMeasure, bool) {
	for _, m := range rec.Measures {
		if m.Key == key {
			return m, true
		}
	}
	return contract.ExploreMeasure{}, false
}

// checkBy checks that every "by" path is a dimension of rec.
func checkBy(rec contract.ExploreRecordType, by []string) error {
	for _, path := range by {
		if !offersDimension(rec, path) {
			return missingPiece(brokenPiece(brokenMissingField, rec.RecordType, path), "%q is not a dimension of %q", path, rec.RecordType)
		}
	}
	return nil
}

// offersDimension reports whether rec offers path as a dimension.
func offersDimension(rec contract.ExploreRecordType, path string) bool {
	for _, d := range rec.Dimensions {
		if d.Path == path {
			return true
		}
	}
	return false
}
