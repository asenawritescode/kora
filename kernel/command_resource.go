package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"gopkg.in/yaml.v3"
)

// CommandResource is a config-defined business command (KERNEL-008, spec §19):
// a named, versioned mutation with typed input, optional preconditions,
// transaction steps over records, and emitted events. Definitions load from
// application configuration exactly like doctypes — no code generation.
//
// Slice limitations (documented, not silent):
//   - update steps resolve their target by name equality only;
//   - preconditions are rejected because this runtime has no evaluator.
type CommandResource struct {
	Name           string                             `yaml:"name"                     json:"name"`
	Namespace      string                             `yaml:"namespace"                json:"namespace,omitempty"`
	Version        int                                `yaml:"version"                  json:"version"`
	Permission     string                             `yaml:"permission"               json:"permission"` // permission-matrix operation, default "create"
	Input          CommandInput                       `yaml:"input"                    json:"input"`
	ModuleOwnerID  contract.ModuleID                  `yaml:"module_owner_id,omitempty" json:"module_owner_id,omitempty"`
	Capability     contract.CapabilityName            `yaml:"capability,omitempty" json:"capability,omitempty"`
	Operation      string                             `yaml:"operation,omitempty" json:"operation,omitempty"`
	InputSchema    contract.SchemaRef                 `yaml:"input_schema,omitempty" json:"input_schema,omitempty"`
	OutputSchema   contract.SchemaRef                 `yaml:"output_schema,omitempty" json:"output_schema,omitempty"`
	CrossEntity    *CrossEntityCommand                `yaml:"cross_entity,omitempty"   json:"cross_entity,omitempty"`
	Preconditions  []string                           `yaml:"preconditions"            json:"preconditions,omitempty"`
	Require        []CommandGuard                     `yaml:"require,omitempty"        json:"require,omitempty"`
	Compute        ComputeBlock                       `yaml:"compute,omitempty"        json:"compute,omitempty"`
	Output         map[string]string                  `yaml:"output,omitempty"         json:"output,omitempty"`
	Steps          []CommandStep                      `yaml:"transaction"              json:"transaction"`
	Emits          []string                           `yaml:"emit"                     json:"emit,omitempty"`
	BusinessEvents []contract.BusinessEventDefinition `yaml:"business_events" json:"business_events,omitempty"`
	Labels         map[string]string                  `yaml:"labels"                   json:"labels,omitempty"`

	// Presentation and OfflineEligible are published with the operation.
	Presentation    *CommandPresentation `yaml:"presentation,omitempty" json:"presentation,omitempty"`
	OfflineEligible bool                 `yaml:"offline_eligible,omitempty" json:"offline_eligible,omitempty"`
}

// CommandPresentation is a SurfacePresentation read from YAML through its
// JSON keys, so the wire type needs no yaml tags.
type CommandPresentation contract.SurfacePresentation

func (p *CommandPresentation) UnmarshalYAML(node *yaml.Node) error {
	var generic any
	if err := node.Decode(&generic); err != nil {
		return err
	}
	raw, err := json.Marshal(generic)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode((*contract.SurfacePresentation)(p))
}

// CrossEntityCommand registers a command that requires exact grants in both
// the invoking entity and a target entity named in its typed input. The
// invoking entity always comes from verified server context.
type CrossEntityCommand struct {
	Resource          string `yaml:"resource"            json:"resource"`
	Operation         string `yaml:"operation"           json:"operation"`
	TargetEntityField string `yaml:"target_entity_field" json:"target_entity_field"`
}

// CommandInput types the payload. The slice form binds one record kind;
// collection-based inputs arrive with SPEC-003.
type CommandInput struct {
	Record string `yaml:"record" json:"record"`
	// Extra declares operation inputs that are not fields of Record, so the
	// published input schema can type them.
	Extra map[string]CommandExtraInput `yaml:"extra,omitempty" json:"extra,omitempty"`
}

// CommandExtraInput types one input that is not a record field. Type is a
// JSON Schema primitive.
type CommandExtraInput struct {
	Type     string `yaml:"type"               json:"type"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// CommandStep is one transactional write. Values map target record fields to
// literal values or "$input.field" references into the operation payload.
type CommandStep struct {
	// ForEach repeats the step once per object in an input list; values may
	// then reference the element as $item.field.
	ForEach string `yaml:"for_each,omitempty" json:"for_each,omitempty"`
	// Where keeps only the for_each elements whose fields all equal these
	// values; a value is a literal or an $input/$steps/$computed reference.
	Where map[string]string `yaml:"where,omitempty" json:"where,omitempty"`
	// As names the record this step writes so later steps can reference it
	// as $steps.<as>.name.
	As     string      `yaml:"as,omitempty" json:"as,omitempty"`
	Create *StepCreate `yaml:"create,omitempty" json:"create,omitempty"`
	Update *StepUpdate `yaml:"update,omitempty" json:"update,omitempty"`
}

type StepCreate struct {
	Record string            `yaml:"record"        json:"record"`
	Scope  string            `yaml:"business_scope" json:"business_scope,omitempty"`
	Values map[string]string `yaml:"values"         json:"values"`
}

type StepUpdate struct {
	Record string `yaml:"record"        json:"record"`
	Name   string `yaml:"name"          json:"name"` // "$input.field" reference
	// Where finds the single target by declared fields when Name is not set.
	Where  map[string]string `yaml:"where,omitempty" json:"where,omitempty"`
	Scope  string            `yaml:"business_scope" json:"business_scope,omitempty"`
	Values map[string]string `yaml:"values"         json:"values"`
	// Adjust changes numeric fields relative to their stored value.
	Adjust map[string]FieldAdjust `yaml:"adjust,omitempty" json:"adjust,omitempty"`
}

// ParseCommandResource parses one command definition from YAML bytes with
// strict unknown-key rejection (KnownFields) so unsupported configuration is
// never silently discarded.
func ParseCommandResource(raw []byte) (*CommandResource, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var def CommandResource
	if err := dec.Decode(&def); err != nil {
		return nil, fmt.Errorf("command definition: %w", err)
	}
	if err := def.validateWith(false); err != nil {
		return nil, err
	}
	return &def, nil
}

// validateWith checks the definition. With needSchemas false (config YAML) the
// versioned schema refs may be absent: the pack generator fills them.
func (c *CommandResource) validateWith(needSchemas bool) error {
	if c.Name == "" {
		return fmt.Errorf("command definition: missing name")
	}
	if len(c.Preconditions) != 0 {
		return fmt.Errorf("command %q: free-form preconditions are not supported; use require", c.FullName())
	}
	if strings.Count(c.Name, ".") == 0 && c.Namespace == "" {
		return fmt.Errorf("command %q: namespaced name or namespace required", c.Name)
	}
	if len(c.Steps) == 0 {
		return fmt.Errorf("command %q: empty transaction", c.Name)
	}
	for i, s := range c.Steps {
		switch {
		case s.Create != nil:
			if s.Create.Record == "" || len(s.Create.Values) == 0 {
				return fmt.Errorf("command %q: step %d create requires record and values", c.Name, i)
			}
		case s.Update != nil:
			if s.Update.Record == "" || (s.Update.Name == "") == (len(s.Update.Where) == 0) || len(s.Update.Values)+len(s.Update.Adjust) == 0 {
				return fmt.Errorf("command %q: step %d update requires record, exactly one of name or where, and values or adjust", c.Name, i)
			}
			for field, change := range s.Update.Adjust {
				if _, set := s.Update.Values[field]; set {
					return fmt.Errorf("command %q: step %d sets and adjusts %q", c.Name, i, field)
				}
				if err := change.validate(); err != nil {
					return fmt.Errorf("command %q: step %d %s: %w", c.Name, i, field, err)
				}
			}
		default:
			return fmt.Errorf("command %q: step %d has neither create nor update", c.Name, i)
		}
	}
	if c.Input.Record == "" {
		return fmt.Errorf("command %q: input.record required in this slice", c.Name)
	}
	for name, extra := range c.Input.Extra {
		switch extra.Type {
		case "string", "integer", "number", "boolean":
		default:
			return fmt.Errorf("command %q: input.extra.%s type %q must be string, integer, number or boolean", c.Name, name, extra.Type)
		}
	}
	if err := c.validateCompute(); err != nil {
		return err
	}
	if err := c.validateStepReferences(); err != nil {
		return err
	}
	if c.ModuleOwnerID != "" || c.Capability != "" || c.Operation != "" || c.InputSchema.ID != "" || c.InputSchema.Version != 0 || c.OutputSchema.ID != "" || c.OutputSchema.Version != 0 {
		noSchemas := !needSchemas && c.InputSchema == (contract.SchemaRef{}) && c.OutputSchema == (contract.SchemaRef{})
		if c.ModuleOwnerID == "" || c.Capability == "" || c.Operation == "" || !noSchemas && (c.InputSchema.ID == "" || c.InputSchema.Version == 0 || c.OutputSchema.ID == "" || c.OutputSchema.Version == 0) {
			return fmt.Errorf("command %q: module capability metadata requires owner, capability, operation, and versioned input/output schemas", c.FullName())
		}
	}
	if len(c.BusinessEvents) > 0 && c.ModuleOwnerID == "" {
		return fmt.Errorf("command %q: authoritative business events require a module owner", c.FullName())
	}
	seenEvents := map[string]bool{}
	for _, event := range c.BusinessEvents {
		if err := contract.ValidateBusinessEvent(event, c.ModuleOwnerID, event.PayloadSchema, event.Type); err != nil {
			return fmt.Errorf("command %q: invalid business event: %w", c.FullName(), err)
		}
		if seenEvents[event.Type] {
			return fmt.Errorf("command %q: duplicate business event %q", c.FullName(), event.Type)
		}
		seenEvents[event.Type] = true
	}
	if c.CrossEntity != nil {
		if c.CrossEntity.Resource == "" || c.CrossEntity.Operation == "" || !strings.HasPrefix(c.CrossEntity.TargetEntityField, "input.") || len(strings.TrimPrefix(c.CrossEntity.TargetEntityField, "input.")) == 0 {
			return fmt.Errorf("command %q: cross_entity requires resource, operation, and target_entity_field under input", c.Name)
		}
		probe := contract.CrossEntityOperation{OperationID: c.FullName(), TenantID: "tenant-a", SourceEntityID: "source", TargetEntityID: "target", Resource: c.CrossEntity.Resource, Operation: c.CrossEntity.Operation}
		if err := probe.Validate(); err != nil {
			return fmt.Errorf("command %q: cross_entity registration is invalid: %w", c.Name, err)
		}
		for i, step := range c.Steps {
			scope := commandStepScope(step)
			if scope != "source" && scope != "target" {
				return fmt.Errorf("command %q: cross-entity step %d requires business_scope source or target", c.Name, i)
			}
		}
	} else {
		for i, step := range c.Steps {
			if scope := commandStepScope(step); scope != "" {
				return fmt.Errorf("command %q: step %d business_scope requires cross_entity registration", c.Name, i)
			}
		}
	}
	return nil
}

func commandStepScope(step CommandStep) string {
	if step.Create != nil {
		return step.Create.Scope
	}
	if step.Update != nil {
		return step.Update.Scope
	}
	return ""
}

// TouchedRecords returns the distinct record kinds the command mutates, in
// first-touch order. Authorization evaluates every one of them.
func (c *CommandResource) TouchedRecords() []string {
	var out []string
	seen := map[string]bool{}
	add := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if c.Input.Record != "" {
		add(c.Input.Record)
	}
	for _, s := range c.Steps {
		if s.Create != nil {
			add(s.Create.Record)
		}
		if s.Update != nil {
			add(s.Update.Record)
		}
	}
	return out
}

// PermOperation is the permission-matrix operation this command requires.
func (c *CommandResource) PermOperation() string {
	if c.Permission == "" {
		return "create"
	}
	return c.Permission
}

// CommandRegistry holds generation-scoped command definitions. It is safe for
// concurrent reads after loading; Register is intended during activation only.
type CommandRegistry struct {
	defs map[string]*CommandResource
}

// CommandSchemaRegistry is the existing versioned payload-schema contract
// injected into the kernel for module-owned commands.
type CommandSchemaRegistry interface {
	Registered(contract.SchemaRef) bool
	Validate(contract.SchemaRef, json.RawMessage) error
}

// NewCommandRegistry returns an empty registry.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{defs: map[string]*CommandResource{}}
}

// Register adds a definition; duplicate full names are rejected with a typed
// error so two packs cannot silently shadow each other.
func (r *CommandRegistry) Register(def *CommandResource) error {
	if def == nil {
		return fmt.Errorf("command registry: nil definition")
	}
	key := def.FullName()
	if _, exists := r.defs[key]; exists {
		return fmt.Errorf("command registry: duplicate command %q", key)
	}
	if def.Capability != "" {
		for _, registered := range r.defs {
			if registered.Capability == def.Capability && registered.Operation == def.Operation {
				return fmt.Errorf("command registry: capability %q operation %q is already assigned to command %q", def.Capability, def.Operation, registered.FullName())
			}
		}
	}
	if r.defs == nil {
		r.defs = map[string]*CommandResource{}
	}
	r.defs[key] = def
	return nil
}

// Lookup resolves a command by full name.
func (r *CommandRegistry) Lookup(name string) (*CommandResource, bool) {
	if r == nil {
		return nil, false
	}
	d, ok := r.defs[name]
	return d, ok
}

// LookupCapability resolves the unique owner command attached to a declared
// capability operation. Definitions remain owned by CommandRegistry.
func (r *CommandRegistry) LookupCapability(capability contract.CapabilityName, operation string) (*CommandResource, bool) {
	if r == nil || capability == "" || operation == "" {
		return nil, false
	}
	for _, definition := range r.defs {
		if definition.Capability == capability && definition.Operation == operation {
			return definition, true
		}
	}
	return nil, false
}

// OwnerCommands returns registered owner-authored domain commands. Public
// transport adapters use it to ensure those commands only enter Kernel via
// their published capability binding, never by a guessed command name.
func (r *CommandRegistry) OwnerCommands() []*CommandResource {
	if r == nil {
		return nil
	}
	var out []*CommandResource
	for _, definition := range r.defs {
		if definition.ModuleOwnerID != "" {
			out = append(out, definition)
		}
	}
	return out
}

// List returns all registered definitions sorted by name for stable
// introspection output.
func (r *CommandRegistry) List() []*CommandResource {
	if r == nil {
		return nil
	}
	out := make([]*CommandResource, 0, len(r.defs))
	for _, d := range r.defs {
		out = append(out, d)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].FullName() < out[j-1].FullName(); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// FullName is the canonical registry key: the namespace prefixes the name
// unless the name already carries it ("livestock.animal.register").
func (c *CommandResource) FullName() string {
	if c.Namespace != "" && !strings.HasPrefix(c.Name, c.Namespace+".") {
		return c.Namespace + "." + c.Name
	}
	return c.Name
}
