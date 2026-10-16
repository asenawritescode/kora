// Package contract — semantic UI manifest schema (UI-001).
//
// The server describes *what* a screen means (pages, regions, blocks, actions,
// states); any ES-module renderer decides *how* to draw it. This schema is
// framework-neutral by construction: no React/Vue/Svelte/DOM identifier appears
// in it. Manifests are inert data — actions reference kernel command names,
// never HTTP paths or JavaScript handlers.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// PageManifest is the pure-data description of one page.
//
//contract:wire
type PageManifest struct {
	SchemaVersion string         `json:"schema_version"`
	Page          string         `json:"page"`
	Title         string         `json:"title"`
	Regions       []Region       `json:"regions"`
	States        ManifestStates `json:"states"`
}

// Region is a named layout area containing blocks.
//
//contract:wire
type Region struct {
	Name   string  `json:"name"`
	Blocks []Block `json:"blocks"`
	Layout Layout  `json:"layout"`
}

// Layout records responsive column spans per breakpoint.
//
//contract:wire
type Layout struct {
	Desktop int `json:"desktop"`
	Tablet  int `json:"tablet"`
	Mobile  int `json:"mobile"`
}

// Block is one component of a region.
//
//contract:wire
type Block struct {
	Kind        string      `json:"kind"` // record | collection | form | custom-es-module
	Collection  string      `json:"collection,omitempty"`
	Fields      []FieldRef  `json:"fields,omitempty"`
	Actions     []ActionRef `json:"actions,omitempty"`
	FeatureFlag string      `json:"feature_flag,omitempty"`
}

// FieldRef references one field by name.
//
//contract:wire
type FieldRef struct {
	Name string `json:"name"`
}

// ActionRef references a kernel command to invoke. IdempotencyKey marks the
// action as eligible for optimistic UI; Requires is the capability scope the
// action needs.
//
//contract:wire
type ActionRef struct {
	Label          string `json:"label"`
	Command        string `json:"command"`
	IdempotencyKey bool   `json:"idempotency_key"`
	Requires       string `json:"requires,omitempty"`
}

// ManifestStates declares the standard UI state rendering tokens.
type ManifestStates struct {
	Loading string `json:"loading"`
	Empty   string `json:"empty"`
	Error   string `json:"error"`
	Offline string `json:"offline"`
}

// ErrUIManifestInvalid is a typed validation failure carrying the key path and
// a human detail.
type ErrUIManifestInvalid struct {
	Path   string
	Detail string
}

func (e *ErrUIManifestInvalid) Error() string {
	return fmt.Sprintf("ui manifest %s: %s", e.Path, e.Detail)
}

var blockKinds = map[string]bool{
	"record": true, "collection": true, "form": true, "custom-es-module": true,
}

var unknownFieldRe2 = regexp.MustCompile(`unknown field "([^"]+)"`)

// Validate checks required fields, block kinds, and action commands. It
// returns the first failure as a typed *ErrUIManifestInvalid.
func (m PageManifest) Validate() error {
	if m.SchemaVersion == "" {
		return &ErrUIManifestInvalid{Path: "schema_version", Detail: "is required"}
	}
	if m.Page == "" {
		return &ErrUIManifestInvalid{Path: "page", Detail: "is required"}
	}
	if m.Title == "" {
		return &ErrUIManifestInvalid{Path: "title", Detail: "is required"}
	}
	regions := map[string]bool{}
	for i, r := range m.Regions {
		if r.Name == "" {
			return &ErrUIManifestInvalid{Path: fmt.Sprintf("regions[%d].name", i), Detail: "is required"}
		}
		if regions[r.Name] {
			return &ErrUIManifestInvalid{Path: fmt.Sprintf("regions[%d].name", i), Detail: fmt.Sprintf("duplicate region %q", r.Name)}
		}
		regions[r.Name] = true
		for j, b := range r.Blocks {
			path := fmt.Sprintf("regions[%d].blocks[%d]", i, j)
			if !blockKinds[b.Kind] {
				return &ErrUIManifestInvalid{Path: path + ".kind", Detail: fmt.Sprintf("unknown block kind %q", b.Kind)}
			}
			for k, a := range b.Actions {
				if a.Command == "" {
					return &ErrUIManifestInvalid{Path: fmt.Sprintf("%s.actions[%d].command", path, k), Detail: "is required"}
				}
			}
		}
	}
	return nil
}

// LoadPageManifestJSON decodes a manifest strictly (unknown fields rejected)
// and validates it.
func LoadPageManifestJSON(data []byte) (*PageManifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m PageManifest
	if err := dec.Decode(&m); err != nil {
		if field := unknownFieldRe2.FindStringSubmatch(err.Error()); len(field) == 2 {
			return nil, &ErrUIManifestInvalid{Path: field[1], Detail: "unknown field"}
		}
		return nil, fmt.Errorf("ui manifest: parse: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
