package configstore

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func (s *Store) SavePageManifest(manifest *doctype.PageManifest, site string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := s.SavePageManifestTx(tx, manifest, site, 0); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SavePageManifestTx(tx *sql.Tx, manifest *doctype.PageManifest, site string, idx int) error {
	if manifest == nil {
		return nil
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	manifest.EnsurePrimaryDataBindings()
	view := manifest.ToView()
	configBytes, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshaling page manifest %q: %w", manifest.Metadata.Name, err)
	}

	publicComponents := ""
	publicAllowMutations := 0
	if view.PublicAccess != nil && view.PublicAccess.Enabled {
		compBytes, _ := json.Marshal(view.PublicAccess.Components)
		publicComponents = string(compBytes)
		if view.PublicAccess.AllowMutations {
			publicAllowMutations = 1
		}
	}

	_, err = tx.Exec(
		fmt.Sprintf(
			`INSERT INTO _kora_view (name, site, route, type, layout, label, module,
				source_doctype, public_enabled, public_components, public_allow_mutations,
				config_json, idx)
			 VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
			 %s`,
			s.Dialect.Placeholder(1), s.Dialect.Placeholder(2), s.Dialect.Placeholder(3),
			s.Dialect.Placeholder(4), s.Dialect.Placeholder(5), s.Dialect.Placeholder(6),
			s.Dialect.Placeholder(7), s.Dialect.Placeholder(8), s.Dialect.Placeholder(9),
			s.Dialect.Placeholder(10), s.Dialect.Placeholder(11), s.Dialect.Placeholder(12),
			s.Dialect.Placeholder(13),
			s.Dialect.UpsertClause(
				[]string{"name"},
				[]string{"route", "type", "layout", "label", "module",
					"source_doctype", "public_enabled", "public_components",
					"public_allow_mutations", "config_json", "idx"},
			),
		),
		view.Name, site, view.Route, "page_manifest", view.Layout, view.Label, view.Module,
		view.SourceDocType, boolToInt(view.PublicAccess != nil && view.PublicAccess.Enabled),
		publicComponents, publicAllowMutations,
		string(configBytes), idx,
	)
	if err != nil {
		return fmt.Errorf("saving page manifest %q: %w", manifest.Metadata.Name, err)
	}
	return nil
}

func (s *Store) LoadPageManifests(site string) ([]*doctype.PageManifest, error) {
	rows, err := s.DB.Query(
		db.Rebind(s.Dialect, `SELECT name, route, type, layout, label, module, source_doctype,
			public_enabled, public_components, public_allow_mutations, config_json
		 FROM _kora_view WHERE site = ? OR site = ''
		 ORDER BY idx`),
		site,
	)
	if err != nil {
		return nil, fmt.Errorf("querying page manifests: %w", err)
	}
	defer rows.Close()

	var manifests []*doctype.PageManifest
	for rows.Next() {
		manifest, err := scanPageManifest(rows)
		if err != nil {
			return nil, err
		}
		if manifest != nil {
			manifests = append(manifests, manifest)
		}
	}
	return manifests, rows.Err()
}

func (s *Store) LoadPageManifest(name, site string) (*doctype.PageManifest, error) {
	row := s.DB.QueryRow(
		db.Rebind(s.Dialect, `SELECT name, route, type, layout, label, module, source_doctype,
			public_enabled, public_components, public_allow_mutations, config_json
		 FROM _kora_view WHERE name = ? AND (site = ? OR site = '')`),
		name, site,
	)
	manifest, err := scanPageManifest(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("page manifest %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("loading page manifest %q: %w", name, err)
	}
	return manifest, nil
}

type pageManifestScanner interface {
	Scan(dest ...any) error
}

func scanPageManifest(scanner pageManifestScanner) (*doctype.PageManifest, error) {
	var (
		name, route, vtype, layout, label, module, sourceDoctype string
		publicEnabled, publicAllowMutations                      int
		publicComponents, configJSON                             sql.NullString
	)
	if err := scanner.Scan(
		&name, &route, &vtype, &layout, &label, &module, &sourceDoctype,
		&publicEnabled, &publicComponents, &publicAllowMutations, &configJSON,
	); err != nil {
		return nil, err
	}

	if configJSON.Valid && isPageManifestJSON(configJSON.String) {
		var manifest doctype.PageManifest
		if err := json.Unmarshal([]byte(configJSON.String), &manifest); err != nil {
			return nil, fmt.Errorf("unmarshaling page manifest %q: %w", name, err)
		}
		manifest.EnsurePrimaryDataBindings()
		return &manifest, nil
	}

	view := &doctype.View{
		Name:          name,
		Route:         route,
		Type:          vtype,
		Layout:        layout,
		Label:         label,
		Module:        module,
		SourceDocType: sourceDoctype,
	}
	if configJSON.Valid && configJSON.String != "" {
		// Views are stored either as a component array or, when they declare
		// resources, as {resources, components}; decodeViewConfig reads both.
		if err := decodeViewConfig(configJSON.String, view); err != nil {
			return nil, fmt.Errorf("decoding view config for page manifest %q: %w", name, err)
		}
	}
	if publicEnabled == 1 {
		pa := &doctype.ViewPublicAccess{Enabled: true}
		if publicComponents.Valid && publicComponents.String != "" {
			_ = json.Unmarshal([]byte(publicComponents.String), &pa.Components)
		}
		pa.AllowMutations = publicAllowMutations == 1
		view.PublicAccess = pa
	}
	manifest := doctype.PageManifestFromView(view)
	manifest.EnsurePrimaryDataBindings()
	return manifest, nil
}
