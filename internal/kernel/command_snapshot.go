package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ParseCommandResourceJSON parses one stored command definition with the same
// strictness as the YAML loader: unknown keys are rejected and the definition
// is validated before it can be registered.
func ParseCommandResourceJSON(raw []byte) (*CommandResource, error) {
	return parseCommandJSON(raw, true)
}

// ParseCommandResourceDraftJSON is ParseCommandResourceJSON for a command whose
// schema refs the pack generator has not filled yet.
func ParseCommandResourceDraftJSON(raw []byte) (*CommandResource, error) {
	return parseCommandJSON(raw, false)
}

func parseCommandJSON(raw []byte, needSchemas bool) (*CommandResource, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var def CommandResource
	if err := dec.Decode(&def); err != nil {
		return nil, fmt.Errorf("command definition: %w", err)
	}
	if err := def.validateWith(needSchemas); err != nil {
		return nil, err
	}
	return &def, nil
}

// EncodeCommandResources converts parsed definitions into the JSON form kept in
// configuration snapshots, so commands version and activate with the rest of a
// tenant's configuration.
func EncodeCommandResources(defs []*CommandResource) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(defs))
	for _, def := range defs {
		raw, err := json.Marshal(def)
		if err != nil {
			return nil, fmt.Errorf("encode command %q: %w", def.FullName(), err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// DecodeCommandResources parses every stored definition and rejects duplicate
// names or capability operations within the set.
func DecodeCommandResources(raw []json.RawMessage) ([]*CommandResource, error) {
	defs := make([]*CommandResource, 0, len(raw))
	check := NewCommandRegistry()
	for i, item := range raw {
		def, err := ParseCommandResourceJSON(item)
		if err != nil {
			return nil, fmt.Errorf("command %d: %w", i, err)
		}
		if err := check.Register(def); err != nil {
			return nil, err
		}
		defs = append(defs, def)
	}
	return defs, nil
}

// Extend returns a new registry containing the receiver's definitions plus
// defs. The receiver is not modified, so a process-wide registry can be shared
// while each tenant generation layers its own configured commands on top.
// Name or capability collisions with existing definitions are rejected.
func (r *CommandRegistry) Extend(defs []*CommandResource) (*CommandRegistry, error) {
	out := NewCommandRegistry()
	if r != nil {
		for _, def := range r.defs {
			out.defs[def.FullName()] = def
		}
	}
	for _, def := range defs {
		if err := out.Register(def); err != nil {
			return nil, err
		}
	}
	return out, nil
}
