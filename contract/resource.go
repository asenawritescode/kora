// Package contract — resource identity and registry contracts (GRAPH-001).
//
// These types give every engine resource a namespaced, versioned identity
// (architecture specification invariant 5) and an explainable dependency
// shape. They are framework-neutral and provider-neutral: the operation
// kernel, component runtime, reconciliation loop, and UI manifest serving all
// address a resource through ResourceRef, never through an ad-hoc doctype
// name + map.
package contract

import (
	"context"
	"strconv"
	"time"
)

// ResourceKind enumerates the resource classes the engine addresses through a
// ResourceRef. New kinds are added only with a contract version bump.
type ResourceKind string

const (
	ResourceKindDoctype    ResourceKind = "doctype"
	ResourceKindCollection ResourceKind = "collection"
	ResourceKindCommand    ResourceKind = "command"
	ResourceKindQuery      ResourceKind = "query"
	// ResourceKindEntity is retained as a source-compatible spelling for
	// callers that used the earlier organizational-runtime vocabulary. It is
	// deliberately the same canonical kind as DocType: Kora has one persisted
	// business-schema primitive, not separate Entity and DocType models.
	ResourceKindEntity      ResourceKind = ResourceKindDoctype
	ResourceKindRelation    ResourceKind = "relation"
	ResourceKindComputation ResourceKind = "computation"
	ResourceKindRule        ResourceKind = "rule"
	ResourceKindPolicy      ResourceKind = "policy"
	ResourceKindAction      ResourceKind = "action"
	ResourceKindWorkflow    ResourceKind = "workflow"
	ResourceKindCapability  ResourceKind = "capability"
	ResourceKindSkill       ResourceKind = "skill"
	ResourceKindRole        ResourceKind = "role"
	ResourceKindAgent       ResourceKind = "agent"
	ResourceKindView        ResourceKind = "view"
	ResourceKindEvent       ResourceKind = "event"
	ResourceKindIntegration ResourceKind = "integration"
	ResourceKindPlugin      ResourceKind = "plugin"
	ResourceKindPackage     ResourceKind = "package"
	ResourceKindPage        ResourceKind = "page"
	ResourceKindComponent   ResourceKind = "component"
)

type ResourceLifecycle string

const (
	LifecycleDraft    ResourceLifecycle = "draft"
	LifecyclePreview  ResourceLifecycle = "preview"
	LifecycleActive   ResourceLifecycle = "active"
	LifecycleDisabled ResourceLifecycle = "disabled"
	LifecycleRetired  ResourceLifecycle = "retired"
)

// TypedField is the minimal field projection carried on a ResourceDescriptor.
// It is a contract-level representation; doctype.Field projects onto it at the
// registry boundary. Field values never cross a package boundary as
// map[string]any.
type TypedField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required,omitempty"`
	Unique   bool   `json:"unique,omitempty"`
}

// ResourceRef is the namespaced, versioned identity of a resource. Namespace
// is the tenant or system scope and is never empty; Version is monotonic per
// (namespace, name).
type ResourceRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   int    `json:"version"`
}

// String renders a stable, human-readable identity ("namespace/name@version").
func (r ResourceRef) String() string {
	return r.Namespace + "/" + r.Name + "@" + strconv.Itoa(r.Version)
}

// ResourceDescriptor is the immutable desired-state definition of one resource
// version. Hash is the canonical SHA-256 of the descriptor content (excluding
// the Hash field itself).
type ResourceDescriptor struct {
	Ref          ResourceRef       `json:"ref"`
	Kind         ResourceKind      `json:"kind"`
	Hash         string            `json:"hash"`
	Fields       []TypedField      `json:"fields,omitempty"`
	DependsOn    []ResourceRef     `json:"depends_on,omitempty"`
	Lifecycle    ResourceLifecycle `json:"lifecycle,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	SupersededAt time.Time         `json:"superseded_at,omitempty"`
}

// ResourceRegistry is the registration and resolution surface for resources.
// Implementations must enforce namespace isolation and monotonic versioning.
// The context is the standard library context; the operation kernel adapts its
// OperationContext into it at the boundary.
type ResourceRegistry interface {
	Register(ctx context.Context, d ResourceDescriptor) (ResourceRef, error)
	Resolve(ref ResourceRef) (ResourceDescriptor, error)
	List(namespace string, kind ResourceKind) ([]ResourceDescriptor, error)
}

// Resource registry sentinel errors. These are stable, typed values; callers
// must match on them (or errors.Is) rather than string-matching messages.
var (
	ErrResourceNamespaceRequired = NewError(CodeValidationFailed, "resource namespace is required")
	ErrResourceNameRequired      = NewError(CodeValidationFailed, "resource name is required")
	ErrResourceNotFound          = NewError(CodeNotFound, "resource not found")
	ErrResourceVersionConflict   = NewError(CodeConflict, "resource version already registered")
	ErrResourceKindUnknown       = NewError(CodeValidationFailed, "unknown resource kind")
)
