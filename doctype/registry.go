package doctype

import (
	"fmt"
	"sync"
)

// ComputedHookFunc is called to evaluate a script-based computed field.
// It receives the doctype name, field name, and document, and returns the computed value.
type ComputedHookFunc func(doctypeName, fieldName string, doc *Document) (any, error)

// Registry holds all DocType definitions, permissions, and workflows.
// It is rebuilt from the active config version on startup and after config changes.
type Registry struct {
	mu              sync.RWMutex
	doctypes        map[string]*DocType // keyed by doctype name
	Permissions     *PermissionMatrix
	Workflows       *WorkflowMap
	ComputedHooks   ComputedHookFunc // optional — runs script-based computed fields
}

// NewRegistry creates an empty registry with blank permission matrix and workflow map.
func NewRegistry() *Registry {
	return &Registry{
		doctypes:    make(map[string]*DocType),
		Permissions: NewPermissionMatrix(),
		Workflows:   NewWorkflowMap(),
	}
}

// Register adds or replaces a DocType in the registry.
func (r *Registry) Register(dt *DocType) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doctypes[dt.Name] = dt
}

// Get returns a DocType by name, or nil if not found.
func (r *Registry) Get(name string) *DocType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.doctypes[name]
}

// Has returns true if the DocType exists in the registry.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.doctypes[name]
	return ok
}

// Unregister removes a DocType from the registry. Safe to call if the doctype
// doesn't exist (no-op). Used to clean up temporary registrations.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.doctypes, name)
}

// All returns all registered DocTypes.
func (r *Registry) All() []*DocType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*DocType, 0, len(r.doctypes))
	for _, dt := range r.doctypes {
		result = append(result, dt)
	}
	return result
}

// Names returns the names of all registered DocTypes.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.doctypes))
	for name := range r.doctypes {
		names = append(names, name)
	}
	return names
}

// ResolveLink resolves a Link field's target DocType.
func (r *Registry) ResolveLink(targetName string) (*DocType, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	dt, ok := r.doctypes[targetName]
	if !ok {
		return nil, fmt.Errorf("link target doctype %q not found in registry", targetName)
	}
	return dt, nil
}

// Len returns the number of registered DocTypes.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.doctypes)
}

// LoadFromDB loads the registry from parsed config data.
func (r *Registry) LoadFromDB(doctypes []*DocType) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doctypes = make(map[string]*DocType, len(doctypes))
	for _, dt := range doctypes {
		r.doctypes[dt.Name] = dt
	}
}

// LoadFull loads doctypes, roles, and permissions into the registry.
func (r *Registry) LoadFull(doctypes []*DocType, roles []*Role, permissions []*Permission) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doctypes = make(map[string]*DocType, len(doctypes))
	for _, dt := range doctypes {
		r.doctypes[dt.Name] = dt
	}
	r.Permissions.LoadPermissionsFromDB(roles, permissions)
}

// GetChildDocType returns the child DocType for a Table field.
func (r *Registry) GetChildDocType(parent *DocType, fieldName string) (*DocType, error) {
	field := parent.GetField(fieldName)
	if field == nil {
		return nil, fmt.Errorf("field %q not found on doctype %s", fieldName, parent.Name)
	}
	if field.Fieldtype != "Table" {
		return nil, fmt.Errorf("field %q on doctype %s is not a Table field", fieldName, parent.Name)
	}
	return r.ResolveLink(field.Options)
}

// Remove removes a DocType from the registry.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.doctypes, name)
}

// CanUser checks permission for a user with given roles.
func (r *Registry) CanUser(userRoles []string, doctype, operation string) (allowed bool, ownerOnly bool) {
	return r.Permissions.UserCan(userRoles, doctype, operation)
}
