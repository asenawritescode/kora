package configstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// SaveViewsTx writes a set of Views to _kora_view within a transaction.
// Existing views for the site are deleted and re-inserted atomically.
func (s *Store) SaveViewsTx(tx *sql.Tx, views []*doctype.View, site string) error {
	// Delete all existing views for this site.
	if _, err := tx.Exec(db.Rebind(s.Dialect, "DELETE FROM _kora_view WHERE site = ?"), site); err != nil {
		return fmt.Errorf("deleting existing views: %w", err)
	}

	for i, v := range views {
		if err := s.saveViewRowTx(tx, v, site, i); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) saveViewRowTx(tx *sql.Tx, v *doctype.View, site string, idx int) error {
	if v == nil {
		return nil
	}

	// Normalize defaults before persisting.
	v.Normalize()

	// Serialize full component tree into config_json.
	configBytes, err := json.Marshal(v.Components)
	if err != nil {
		return fmt.Errorf("marshaling components for view %q: %w", v.Name, err)
	}

	publicComponents := ""
	publicAllowMutations := 0
	if v.PublicAccess != nil && v.PublicAccess.Enabled {
		if len(v.PublicAccess.Components) == 0 {
			return nil
		}
		compBytes, _ := json.Marshal(v.PublicAccess.Components)
		publicComponents = string(compBytes)
		if v.PublicAccess.AllowMutations {
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
		v.Name, site, v.Route, v.Type, v.Layout, v.Label, v.Module,
		v.SourceDocType, boolToInt(v.PublicAccess != nil && v.PublicAccess.Enabled),
		publicComponents, publicAllowMutations,
		string(configBytes), idx,
	)
	if err != nil {
		return fmt.Errorf("saving view %q: %w", v.Name, err)
	}

	return nil
}

// SaveView inserts or updates a single View without replacing other views.
func (s *Store) SaveView(view *doctype.View, site string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := s.saveViewRowTx(tx, view, site, 0); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveViews replaces all views in a single transaction (for import/export batch operations).
func (s *Store) SaveViews(views []*doctype.View, site string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := s.SaveViewsTx(tx, views, site); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadViews reads all Views for a site from _kora_view.
func (s *Store) LoadViews(site string) ([]*doctype.View, error) {
	rows, err := s.DB.Query(db.Rebind(s.Dialect,
		`SELECT name, route, type, layout, label, module, source_doctype,
			public_enabled, public_components, public_allow_mutations, config_json
		 FROM _kora_view WHERE site = ? OR site = ''
		 ORDER BY idx`,
	), site)
	if err != nil {
		return nil, fmt.Errorf("querying views: %w", err)
	}
	defer rows.Close()

	var views []*doctype.View
	for rows.Next() {
		var (
			name, route, vtype, layout, label, module, sourceDoctype string
			publicEnabled, publicAllowMutations                      int
			publicComponents, configJSON                             sql.NullString
		)
		if err := rows.Scan(
			&name, &route, &vtype, &layout, &label, &module, &sourceDoctype,
			&publicEnabled, &publicComponents, &publicAllowMutations, &configJSON,
		); err != nil {
			return nil, fmt.Errorf("scanning view: %w", err)
		}

		v := &doctype.View{
			Name:          name,
			Route:         route,
			Type:          vtype,
			Layout:        layout,
			Label:         label,
			Module:        module,
			SourceDocType: sourceDoctype,
		}

		// Deserialize native PageManifest payloads as their route/navigation
		// projection for older registry consumers.
		if configJSON.Valid && configJSON.String != "" {
			if isPageManifestJSON(configJSON.String) {
				var manifest doctype.PageManifest
				if err := json.Unmarshal([]byte(configJSON.String), &manifest); err != nil {
					return nil, fmt.Errorf("unmarshaling page manifest for view %q: %w", name, err)
				}
				v = manifest.ToView()
			} else if err := json.Unmarshal([]byte(configJSON.String), &v.Components); err != nil {
				return nil, fmt.Errorf("unmarshaling components for view %q: %w", name, err)
			}
		}

		// Deserialize public access.
		if publicEnabled == 1 {
			pa := &doctype.ViewPublicAccess{Enabled: true}
			if publicComponents.Valid && publicComponents.String != "" {
				json.Unmarshal([]byte(publicComponents.String), &pa.Components)
			}
			pa.AllowMutations = publicAllowMutations == 1
			v.PublicAccess = pa
		}

		v.Normalize()
		views = append(views, v)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating views: %w", err)
	}
	return views, nil
}

// LoadView reads a single View by name.
func (s *Store) LoadView(name, site string) (*doctype.View, error) {
	row := s.DB.QueryRow(
		db.Rebind(s.Dialect, `SELECT name, route, type, layout, label, module, source_doctype,
			public_enabled, public_components, public_allow_mutations, config_json
		 FROM _kora_view WHERE name = ? AND (site = ? OR site = '')`),
		name, site,
	)

	var (
		vname, route, vtype, layout, label, module, sourceDoctype string
		publicEnabled, publicAllowMutations                       int
		publicComponents, configJSON                              sql.NullString
	)
	err := row.Scan(
		&vname, &route, &vtype, &layout, &label, &module, &sourceDoctype,
		&publicEnabled, &publicComponents, &publicAllowMutations, &configJSON,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("view %q not found: %w", name, sql.ErrNoRows)
	}
	if err != nil {
		return nil, fmt.Errorf("loading view %q: %w", name, err)
	}

	v := &doctype.View{
		Name:          vname,
		Route:         route,
		Type:          vtype,
		Layout:        layout,
		Label:         label,
		Module:        module,
		SourceDocType: sourceDoctype,
	}

	if configJSON.Valid && configJSON.String != "" {
		if isPageManifestJSON(configJSON.String) {
			var manifest doctype.PageManifest
			if err := json.Unmarshal([]byte(configJSON.String), &manifest); err != nil {
				return nil, fmt.Errorf("unmarshaling page manifest: %w", err)
			}
			v = manifest.ToView()
		} else if err := json.Unmarshal([]byte(configJSON.String), &v.Components); err != nil {
			return nil, fmt.Errorf("unmarshaling components: %w", err)
		}
	}

	if publicEnabled == 1 {
		pa := &doctype.ViewPublicAccess{Enabled: true}
		if publicComponents.Valid && publicComponents.String != "" {
			json.Unmarshal([]byte(publicComponents.String), &pa.Components)
		}
		pa.AllowMutations = publicAllowMutations == 1
		v.PublicAccess = pa
	}

	v.Normalize()
	return v, nil
}

func isPageManifestJSON(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"apiVersion"`) && strings.Contains(trimmed, `"ui.kora.dev/v1"`)
}

// DeleteView removes a View by name.
func (s *Store) DeleteView(name, site string) error {
	_, err := s.DB.Exec(db.Rebind(s.Dialect, "DELETE FROM _kora_view WHERE name = ? AND site = ?"), name, site)
	if err != nil {
		return fmt.Errorf("deleting view %q: %w", name, err)
	}
	return nil
}
