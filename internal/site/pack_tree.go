package site

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

const packFileName = "pack.yaml"

// ParsePackFile reads the optional config/<pack>/pack.yaml. The bool is false
// when the file is absent.
func ParsePackFile(dir string) (contract.PackManifest, bool, error) {
	body, err := os.ReadFile(filepath.Join(dir, packFileName))
	if errors.Is(err, os.ErrNotExist) {
		return contract.PackManifest{}, false, nil
	}
	if err != nil {
		return contract.PackManifest{}, false, fmt.Errorf("read %s: %w", packFileName, err)
	}
	return parsePackManifest(body, filepath.Base(filepath.Clean(dir)))
}

// parsePackManifest decodes pack.yaml bytes; name must equal the pack name.
func parsePackManifest(body []byte, dirName string) (contract.PackManifest, bool, error) {
	var raw struct {
		Name     string                     `yaml:"name"`
		Version  string                     `yaml:"version"`
		Requires []contract.ManifestPackRef `yaml:"requires"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(body), yaml.Strict()).Decode(&raw); err != nil {
		return contract.PackManifest{}, false, fmt.Errorf("parse %s: %w", packFileName, err)
	}
	if raw.Name != dirName {
		return contract.PackManifest{}, false, fmt.Errorf("%s name %q must equal directory name", packFileName, raw.Name)
	}
	if err := contract.ValidateManifestPackRefs(raw.Requires, nil); err != nil {
		return contract.PackManifest{}, false, fmt.Errorf("%s: %w", packFileName, err)
	}
	return contract.PackManifest{Name: raw.Name, Version: raw.Version, Requires: raw.Requires}, true, nil
}

// ResolvePackTree returns configPath and its transitive required sibling packs,
// depth-first with dependencies first and duplicates removed.
func ResolvePackTree(configPath string) ([]string, error) {
	var order []string
	done := map[string]bool{}
	var visit func(dir string, stack []string) error
	visit = func(dir string, stack []string) error {
		dir = filepath.Clean(dir)
		for _, s := range stack {
			if s == dir {
				return fmt.Errorf("pack.requires_cycle: %s", filepath.Base(dir))
			}
		}
		if done[dir] {
			return nil
		}
		manifest, _, err := ParsePackFile(dir)
		if err != nil {
			return err
		}
		for _, ref := range manifest.Requires {
			dep := filepath.Join(filepath.Dir(dir), ref.PackageID)
			if info, err := os.Stat(dep); err != nil || !info.IsDir() {
				return fmt.Errorf("pack.requires_missing: %s requires %s", manifest.Name, ref.PackageID)
			}
			depManifest, _, err := ParsePackFile(dep)
			if err != nil {
				return err
			}
			if ref.Version != "" && depManifest.Version != ref.Version {
				return fmt.Errorf("pack.requires_version: %s requires %s %q, found %q", manifest.Name, ref.PackageID, ref.Version, depManifest.Version)
			}
			if err := visit(dep, append(stack, dir)); err != nil {
				return err
			}
		}
		done[dir] = true
		order = append(order, dir)
		return nil
	}
	if err := visit(configPath, nil); err != nil {
		return nil, err
	}
	return order, nil
}

// mergePackSnapshot folds next into dst: duplicate DocType/command/view/workflow
// names conflict, the first role definition wins, permissions concatenate.
func mergePackSnapshot(dst, next *doctype.ConfigSnapshot, names map[string]bool) error {
	claim := func(kind, name string) error {
		key := kind + "\x00" + name
		if names[key] {
			return fmt.Errorf("pack.resource_conflict: %s %q defined in more than one pack", kind, name)
		}
		names[key] = true
		return nil
	}
	for _, d := range next.DocTypes {
		if err := claim("doctype", d.Name); err != nil {
			return err
		}
	}
	for _, v := range next.Views {
		if err := claim("view", v.Name); err != nil {
			return err
		}
	}
	for _, w := range next.WorkflowDefinitions {
		if err := claim("workflow", string(w.ID)); err != nil {
			return err
		}
	}
	for _, c := range next.Commands {
		var head struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(c, &head); err != nil {
			return fmt.Errorf("decode command: %w", err)
		}
		if err := claim("command", head.Name); err != nil {
			return err
		}
	}
	for _, m := range next.Modules {
		if err := claim("module", string(m.ID)); err != nil {
			return err
		}
	}
	have := map[string]bool{}
	for _, r := range dst.Roles {
		have[r.Name] = true
	}
	for _, r := range next.Roles {
		if !have[r.Name] {
			dst.Roles = append(dst.Roles, r)
		}
	}
	dst.DocTypes = append(dst.DocTypes, next.DocTypes...)
	dst.Permissions = append(dst.Permissions, next.Permissions...)
	dst.Views = append(dst.Views, next.Views...)
	dst.Reports = append(dst.Reports, next.Reports...)
	dst.Commands = append(dst.Commands, next.Commands...)
	dst.Modules = append(dst.Modules, next.Modules...)
	dst.Queries = append(dst.Queries, next.Queries...)
	dst.PrintFormats = append(dst.PrintFormats, next.PrintFormats...)
	dst.WorkflowDefinitions = append(dst.WorkflowDefinitions, next.WorkflowDefinitions...)
	return nil
}
