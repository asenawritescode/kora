package configstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// SaveViewsTx writes a set of Views to _kora_view within a transaction.
// Existing views for the site are deleted and re-inserted atomically.
func (s *Store) SaveViewsTx(tx *sql.Tx, views []*doctype.View, site string) error {
	// Delete all existing views for this site.
	if _, err := tx.Exec(db.Rebind(s.Dialect, "DELETE FROM _kora_view WHERE site = ?"), site); err != nil {
		return fmt.Errorf("deleting existing views: %w", err)
	}

	for i, v := range views {
		if v != nil && v.PageManifest != nil {
			if err := s.SavePageManifestTx(tx, v.PageManifest, site, i); err != nil {
				return err
			}
			continue
		}
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

	// Serialize the component tree (and declared resources) into config_json.
	configBytes, err := encodeViewConfig(v)
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
	if err := s.SaveViewTx(tx, view, site); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveViewTx writes one View using a caller-owned transaction so callers can
// commit the persisted View and its config-version snapshot atomically.
func (s *Store) SaveViewTx(tx *sql.Tx, view *doctype.View, site string) error {
	if tx == nil {
		return fmt.Errorf("transaction is required")
	}
	if err := s.saveViewRowTx(tx, view, site, 0); err != nil {
		return err
	}
	return BumpConfigRevisionTx(tx, s.Dialect)
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
	if err := BumpConfigRevisionTx(tx, s.Dialect); err != nil {
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
			} else if err := decodeViewConfig(configJSON.String, v); err != nil {
				return nil, fmt.Errorf("unmarshaling components for view %q: %w", name, err)
			}
		}

		// Deserialize public access.
		if publicEnabled == 1 {
			pa := &doctype.ViewPublicAccess{Enabled: true}
			if publicComponents.Valid && publicComponents.String != "" {
				decodePublicComponents(publicComponents.String, pa)
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
		} else if err := decodeViewConfig(configJSON.String, v); err != nil {
			return nil, fmt.Errorf("unmarshaling components: %w", err)
		}
	}

	if publicEnabled == 1 {
		pa := &doctype.ViewPublicAccess{Enabled: true}
		if publicComponents.Valid && publicComponents.String != "" {
			decodePublicComponents(publicComponents.String, pa)
		}
		pa.AllowMutations = publicAllowMutations == 1
		v.PublicAccess = pa
	}

	v.Normalize()
	return v, nil
}

// viewConfig is the config_json shape of a view that declares resources.
// Views without resources keep the historic component array.
type viewConfig struct {
	Resources  []doctype.PageResource  `json:"resources"`
	Components []doctype.ViewComponent `json:"components"`
}

func encodeViewConfig(v *doctype.View) ([]byte, error) {
	if len(v.Resources) == 0 {
		return json.Marshal(v.Components)
	}
	return json.Marshal(viewConfig{Resources: v.Resources, Components: v.Components})
}

func decodeViewConfig(raw string, v *doctype.View) error {
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return json.Unmarshal([]byte(raw), &v.Components)
	}
	var config viewConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return err
	}
	v.Resources, v.Components = config.Resources, config.Components
	return nil
}

func isPageManifestJSON(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"apiVersion"`) && strings.Contains(trimmed, `"ui.kora.dev/v1"`)
}

// DeleteView removes a View by name.
func (s *Store) DeleteView(name, site string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin view deletion: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(db.Rebind(s.Dialect, "DELETE FROM _kora_view WHERE name = ? AND site = ?"), name, site); err != nil {
		return fmt.Errorf("deleting view %q: %w", name, err)
	}
	if err := BumpConfigRevisionTx(tx, s.Dialect); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit view deletion: %w", err)
	}
	return nil
}

// decodePublicComponents loads a stored public-component list. Corrupt JSON leaves the list empty,
// which fails ViewPublicAccess.Validate, so a damaged row can never widen what a view exposes.
func decodePublicComponents(raw string, pa *doctype.ViewPublicAccess) {
	if err := json.Unmarshal([]byte(raw), &pa.Components); err != nil {
		slog.Warn("stored view public components are not valid JSON; public access left empty", "error", err)
		pa.Components = nil
	}
}
