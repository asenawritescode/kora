package doctype

import "sync"

// ViewRegistry provides fast lookup of Views by name, route, and module.
// It is safe for concurrent use.
type ViewRegistry struct {
	mu      sync.RWMutex
	views   map[string]*View // name → view
	byRoute map[string]*View // route → view
}

// NewViewRegistry creates an empty view registry.
func NewViewRegistry() *ViewRegistry {
	return &ViewRegistry{
		views:   make(map[string]*View),
		byRoute: make(map[string]*View),
	}
}

// Register adds or replaces a View in the registry.
func (vr *ViewRegistry) Register(v *View) {
	vr.mu.Lock()
	defer vr.mu.Unlock()
	v.Normalize()
	vr.views[v.Name] = v
	if v.Route != "" {
		vr.byRoute[v.Route] = v
	}
}

// GetByName returns a View by name, or nil if not found.
func (vr *ViewRegistry) GetByName(name string) *View {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	return vr.views[name]
}

// GetByRoute returns a View by route, or nil if not found.
func (vr *ViewRegistry) GetByRoute(route string) *View {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	return vr.byRoute[route]
}

// GetByModule returns all Views belonging to a module.
func (vr *ViewRegistry) GetByModule(module string) []*View {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	var result []*View
	for _, v := range vr.views {
		if v.Module == module {
			result = append(result, v)
		}
	}
	return result
}

// All returns all registered Views.
func (vr *ViewRegistry) All() []*View {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	result := make([]*View, 0, len(vr.views))
	for _, v := range vr.views {
		result = append(result, v)
	}
	return result
}

// Len returns the number of registered Views.
func (vr *ViewRegistry) Len() int {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	return len(vr.views)
}

// Remove removes a View from the registry by name.
func (vr *ViewRegistry) Remove(name string) {
	vr.mu.Lock()
	defer vr.mu.Unlock()
	if v, ok := vr.views[name]; ok {
		if v.Route != "" {
			delete(vr.byRoute, v.Route)
		}
	}
	delete(vr.views, name)
}

// LoadFromDB loads Views from a database snapshot into the registry.
// Existing entries are replaced.
func (vr *ViewRegistry) LoadFromDB(views []*View) {
	vr.mu.Lock()
	defer vr.mu.Unlock()
	vr.views = make(map[string]*View, len(views))
	vr.byRoute = make(map[string]*View, len(views))
	for _, v := range views {
		v.Normalize()
		vr.views[v.Name] = v
		if v.Route != "" {
			vr.byRoute[v.Route] = v
		}
	}
}
