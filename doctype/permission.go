package doctype

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// AdminRole is the role name that bypasses all permission checks. Defaults to AdminRole.
var AdminRole = "Administrator"

// SetAdminRole sets the admin role name from config.
func SetAdminRole(role string) { AdminRole = role }

// Role definition from roles.yaml
type Role struct {
	Name            string `yaml:"name"        json:"name"`
	WorkspaceAccess bool   `yaml:"workspace_access" json:"workspace_access"`
	Description     string `yaml:"description" json:"description"`
}

// Permission definition from permissions.yaml
type Permission struct {
	Doctype string `yaml:"doctype"   json:"doctype"`
	Role    string `yaml:"role"      json:"role"`
	Read    bool   `yaml:"read"      json:"read"`
	Write   bool   `yaml:"write"     json:"write"`
	Create  bool   `yaml:"create"    json:"create"`
	Delete  bool   `yaml:"delete"    json:"delete"`
	Submit  bool   `yaml:"submit"    json:"submit"`
	Cancel  bool   `yaml:"cancel"    json:"cancel"`
	Amend   bool   `yaml:"amend"     json:"amend"`
	Export  bool   `yaml:"export"    json:"export"`
	Import  bool   `yaml:"import"    json:"import"`
	Report  bool   `yaml:"report"    json:"report"`
	IfOwner bool   `yaml:"if_owner"  json:"if_owner"`
	// IfAssigned lists concrete fields whose value may match the verified
	// actor identity to grant the same row access as creator ownership.
	IfAssigned []string `yaml:"if_assigned,omitempty" json:"if_assigned,omitempty"`
}

// UnmarshalYAML accepts both the current short permission keys (read/write)
// and the established v0 package keys (can_read/can_write). Several shipped
// packs use the latter, so silently dropping them would deny configured
// access or make row scopes ineffective.
func (p *Permission) UnmarshalYAML(node *yaml.Node) error {
	type plain Permission
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	var aliases struct {
		CanRead   *bool `yaml:"can_read"`
		CanWrite  *bool `yaml:"can_write"`
		CanCreate *bool `yaml:"can_create"`
		CanDelete *bool `yaml:"can_delete"`
		CanSubmit *bool `yaml:"can_submit"`
		CanCancel *bool `yaml:"can_cancel"`
		CanAmend  *bool `yaml:"can_amend"`
		CanExport *bool `yaml:"can_export"`
		CanImport *bool `yaml:"can_import"`
		CanReport *bool `yaml:"can_report"`
	}
	if err := node.Decode(&aliases); err != nil {
		return err
	}
	pv := Permission(decoded)
	pairs := []struct {
		name   string
		short  *bool
		legacy *bool
		target *bool
	}{
		{"read", boolPtrFromNode(node, "read"), aliases.CanRead, &pv.Read},
		{"write", boolPtrFromNode(node, "write"), aliases.CanWrite, &pv.Write},
		{"create", boolPtrFromNode(node, "create"), aliases.CanCreate, &pv.Create},
		{"delete", boolPtrFromNode(node, "delete"), aliases.CanDelete, &pv.Delete},
		{"submit", boolPtrFromNode(node, "submit"), aliases.CanSubmit, &pv.Submit},
		{"cancel", boolPtrFromNode(node, "cancel"), aliases.CanCancel, &pv.Cancel},
		{"amend", boolPtrFromNode(node, "amend"), aliases.CanAmend, &pv.Amend},
		{"export", boolPtrFromNode(node, "export"), aliases.CanExport, &pv.Export},
		{"import", boolPtrFromNode(node, "import"), aliases.CanImport, &pv.Import},
		{"report", boolPtrFromNode(node, "report"), aliases.CanReport, &pv.Report},
	}
	for _, pair := range pairs {
		if pair.short != nil && pair.legacy != nil && *pair.short != *pair.legacy {
			return fmt.Errorf("conflicting %s and can_%s permission values", pair.name, pair.name)
		}
		if pair.legacy != nil {
			*pair.target = *pair.legacy
		} else if pair.short != nil {
			*pair.target = *pair.short
		}
	}
	*p = pv
	return nil
}

func boolPtrFromNode(node *yaml.Node, key string) *bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			var value bool
			if node.Content[i+1].Decode(&value) == nil {
				return &value
			}
			return nil
		}
	}
	return nil
}

// PermissionMatrix provides fast lookup of permissions.
// role → doctype → Permission
type PermissionMatrix struct {
	mu          sync.RWMutex
	roles       map[string]*Role
	permissions map[string]map[string]*Permission // [role][doctype]
}

// NewPermissionMatrix creates an empty permission matrix.
func NewPermissionMatrix() *PermissionMatrix {
	return &PermissionMatrix{
		roles:       make(map[string]*Role),
		permissions: make(map[string]map[string]*Permission),
	}
}

// AddRole registers a role.
func (pm *PermissionMatrix) AddRole(role *Role) {
	if pm == nil || role == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	copy := *role
	pm.roles[copy.Name] = &copy
}

// GetRole returns a role by name.
func (pm *PermissionMatrix) GetRole(name string) *Role {
	if pm == nil {
		return nil
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	role := pm.roles[name]
	if role == nil {
		return nil
	}
	copy := *role
	return &copy
}

// SetPermission adds or updates a permission entry.
func (pm *PermissionMatrix) SetPermission(p *Permission) {
	if pm == nil || p == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.permissions[p.Role] == nil {
		pm.permissions[p.Role] = make(map[string]*Permission)
	}
	copy := *p
	copy.IfAssigned = append([]string(nil), p.IfAssigned...)
	pm.permissions[copy.Role][copy.Doctype] = &copy
}

// Can checks if a role can perform an operation on a doctype.
func (pm *PermissionMatrix) Can(role, doctype, operation string) bool {
	if pm == nil {
		return false
	}
	if role == AdminRole {
		return true // Admin can do everything.
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	rolePerms, ok := pm.permissions[role]
	if !ok {
		return false
	}
	p, ok := rolePerms[doctype]
	if !ok {
		return false
	}
	switch operation {
	case "read":
		return p.Read
	case "write":
		return p.Write
	case "create":
		return p.Create
	case "delete":
		return p.Delete
	case "submit":
		return p.Submit
	case "cancel":
		return p.Cancel
	case "amend":
		return p.Amend
	case "export":
		return p.Export
	case "import":
		return p.Import
	case "report":
		return p.Report
	}
	return false
}

// IsOwnerOnly returns true if the permission for this role+doctype is scoped to owner.
func (pm *PermissionMatrix) IsOwnerOnly(role, doctype string) bool {
	if pm == nil {
		return false
	}
	if role == AdminRole {
		return false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	rolePerms, ok := pm.permissions[role]
	if !ok {
		return false
	}
	p, ok := rolePerms[doctype]
	if !ok {
		return false
	}
	return p.IfOwner
}

// HasRole checks if a user (identified by a list of roles) has the given role.
func (pm *PermissionMatrix) HasRole(userRoles []string, role string) bool {
	for _, r := range userRoles {
		if r == role || r == AdminRole {
			return true
		}
	}
	return false
}

// UserCan checks if a user with given roles can perform an operation.
// Returns (allowed, isOwnerScoped).
func (pm *PermissionMatrix) UserCan(userRoles []string, doctype, operation string) (bool, bool) {
	for _, role := range userRoles {
		if pm.Can(role, doctype, operation) {
			return true, pm.IsOwnerOnly(role, doctype)
		}
	}
	return false, false
}

// RowScopeFor returns the narrowest matching row scope for an allowed
// permission. Multiple roles are combined restrictively: creator ownership
// and configured assignment fields are unioned, never widened to all rows.
func (pm *PermissionMatrix) RowScopeFor(userRoles []string, doctype, operation string) (bool, []string) {
	if pm == nil {
		return false, nil
	}
	for _, role := range userRoles {
		if role == AdminRole {
			return false, nil
		}
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	var found bool
	owner := false
	fields := map[string]struct{}{}
	for _, role := range userRoles {
		p := pm.permissions[role][doctype]
		if p == nil || !permissionAllows(p, operation) {
			continue
		}
		found = true
		if !p.IfOwner && len(p.IfAssigned) == 0 {
			return false, nil // an unrestricted grant covers every row
		}
		owner = owner || p.IfOwner
		for _, field := range p.IfAssigned {
			if field = strings.TrimSpace(field); field != "" {
				fields[field] = struct{}{}
			}
		}
	}
	if !found {
		return false, nil
	}
	result := make([]string, 0, len(fields))
	for field := range fields {
		result = append(result, field)
	}
	sort.Strings(result)
	return owner, result
}

func permissionAllows(p *Permission, operation string) bool {
	switch operation {
	case "read":
		return p.Read
	case "write":
		return p.Write
	case "create":
		return p.Create
	case "delete":
		return p.Delete
	case "submit":
		return p.Submit
	case "cancel":
		return p.Cancel
	case "amend":
		return p.Amend
	case "export":
		return p.Export
	case "import":
		return p.Import
	case "report":
		return p.Report
	default:
		return false
	}
}

// AllRoles returns all registered roles.
func (pm *PermissionMatrix) AllRoles() []*Role {
	if pm == nil {
		return nil
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	roles := make([]*Role, 0, len(pm.roles))
	for _, r := range pm.roles {
		if r != nil {
			copy := *r
			roles = append(roles, &copy)
		}
	}
	return roles
}

// AllPermissions returns all registered permissions.
func (pm *PermissionMatrix) AllPermissions() []*Permission {
	if pm == nil {
		return nil
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	var perms []*Permission
	for _, rolePerms := range pm.permissions {
		for _, p := range rolePerms {
			if p != nil {
				copy := *p
				copy.IfAssigned = append([]string(nil), p.IfAssigned...)
				perms = append(perms, &copy)
			}
		}
	}
	return perms
}

// Serialize returns the matrix as JSON for storage in config version.
func (pm *PermissionMatrix) Serialize() ([]byte, error) {
	data := map[string]any{
		"roles":       pm.AllRoles(),
		"permissions": pm.AllPermissions(),
	}
	return json.Marshal(data)
}

// LoadPermissionsFromDB loads permissions from DB records.
func (pm *PermissionMatrix) LoadPermissionsFromDB(roles []*Role, permissions []*Permission) {
	if pm == nil {
		return
	}
	newRoles := make(map[string]*Role, len(roles))
	for _, r := range roles {
		if r != nil {
			copy := *r
			newRoles[copy.Name] = &copy
		}
	}
	newPermissions := make(map[string]map[string]*Permission)
	for _, p := range permissions {
		if p == nil {
			continue
		}
		if newPermissions[p.Role] == nil {
			newPermissions[p.Role] = make(map[string]*Permission)
		}
		copy := *p
		copy.IfAssigned = append([]string(nil), p.IfAssigned...)
		newPermissions[copy.Role][copy.Doctype] = &copy
	}
	pm.mu.Lock()
	pm.roles, pm.permissions = newRoles, newPermissions
	pm.mu.Unlock()
}

// PolicyDigest returns a stable digest of the active roles and permissions for
// cache keys. Sorting makes the digest independent of map iteration order.
func (pm *PermissionMatrix) PolicyDigest() string {
	if pm == nil {
		return ""
	}
	pm.mu.RLock()
	roles := make([]Role, 0, len(pm.roles))
	for _, role := range pm.roles {
		if role != nil {
			roles = append(roles, *role)
		}
	}
	permissions := make([]Permission, 0)
	for _, byDoctype := range pm.permissions {
		for _, permission := range byDoctype {
			if permission != nil {
				permissions = append(permissions, *permission)
			}
		}
	}
	pm.mu.RUnlock()
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	sort.Slice(permissions, func(i, j int) bool {
		if permissions[i].Role == permissions[j].Role {
			return permissions[i].Doctype < permissions[j].Doctype
		}
		return permissions[i].Role < permissions[j].Role
	})
	payload, err := json.Marshal(struct {
		Roles       []Role       `json:"roles"`
		Permissions []Permission `json:"permissions"`
	}{Roles: roles, Permissions: permissions})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// ParseRolesFile parses a roles.yaml file.
func ParseRolesFile(path string) ([]*Role, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading roles file: %w", err)
	}
	return ParseRolesYAML(data)
}

// ParseRolesYAML parses roles from YAML data in memory.
func ParseRolesYAML(data []byte) ([]*Role, error) {
	var roles []*Role
	if err := yaml.Unmarshal(data, &roles); err != nil {
		return nil, fmt.Errorf("parsing roles: %w", err)
	}
	for _, r := range roles {
		if r.Name == "" {
			return nil, fmt.Errorf("role has no name")
		}
	}
	return roles, nil
}

// ParsePermissionsFile parses a permissions.yaml file.
func ParsePermissionsFile(path string) ([]*Permission, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading permissions file: %w", err)
	}
	return ParsePermissionsYAML(data)
}

// ParsePermissionsYAML parses permissions from YAML data in memory.
func ParsePermissionsYAML(data []byte) ([]*Permission, error) {
	var perms []*Permission
	if err := yaml.Unmarshal(data, &perms); err != nil {
		return nil, fmt.Errorf("parsing permissions: %w", err)
	}
	for _, p := range perms {
		if p.Doctype == "" || p.Role == "" {
			return nil, fmt.Errorf("permission missing doctype or role")
		}
	}
	return perms, nil
}

// ParseRolesDirectory looks for roles.yaml and permissions.yaml in a config directory.
func ParseRolesDirectory(path string) ([]*Role, []*Permission, error) {
	var roles []*Role
	var permissions []*Permission

	rolesPath := strings.TrimSuffix(path, "/doctypes") + "/roles.yaml"
	if _, err := os.Stat(rolesPath); err == nil {
		r, err := ParseRolesFile(rolesPath)
		if err != nil {
			return nil, nil, err
		}
		roles = r
	}

	permsPath := strings.TrimSuffix(path, "/doctypes") + "/permissions.yaml"
	if _, err := os.Stat(permsPath); err == nil {
		p, err := ParsePermissionsFile(permsPath)
		if err != nil {
			return nil, nil, err
		}
		permissions = p
	}

	// Also check parent directory.
	if len(roles) == 0 {
		parentRoles := strings.TrimSuffix(strings.TrimSuffix(path, "/doctypes"), "/config") + "/roles.yaml"
		if _, err := os.Stat(parentRoles); err == nil {
			r, err := ParseRolesFile(parentRoles)
			if err != nil {
				return nil, nil, err
			}
			roles = r
		}
	}

	return roles, permissions, nil
}
