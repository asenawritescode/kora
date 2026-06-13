package doctype

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AdminRole is the role name that bypasses all permission checks. Defaults to AdminRole.
var AdminRole = "Administrator"

// SetAdminRole sets the admin role name from config.
func SetAdminRole(role string) { AdminRole = role }

// Role definition from roles.yaml
type Role struct {
	Name        string `yaml:"name"        json:"name"`
	WorkspaceAccess bool   `yaml:"workspace_access" json:"workspace_access"`
	Description string `yaml:"description" json:"description"`
}

// Permission definition from permissions.yaml
type Permission struct {
	Doctype   string `yaml:"doctype"   json:"doctype"`
	Role      string `yaml:"role"      json:"role"`
	Read      bool   `yaml:"read"      json:"read"`
	Write     bool   `yaml:"write"     json:"write"`
	Create    bool   `yaml:"create"    json:"create"`
	Delete    bool   `yaml:"delete"    json:"delete"`
	Submit    bool   `yaml:"submit"    json:"submit"`
	Cancel    bool   `yaml:"cancel"    json:"cancel"`
	Amend     bool   `yaml:"amend"     json:"amend"`
	Export    bool   `yaml:"export"    json:"export"`
	Import    bool   `yaml:"import"    json:"import"`
	Report    bool   `yaml:"report"    json:"report"`
	IfOwner   bool   `yaml:"if_owner"  json:"if_owner"`
}

// PermissionMatrix provides fast lookup of permissions.
// role → doctype → Permission
type PermissionMatrix struct {
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
	pm.roles[role.Name] = role
}

// GetRole returns a role by name.
func (pm *PermissionMatrix) GetRole(name string) *Role {
	return pm.roles[name]
}

// SetPermission adds or updates a permission entry.
func (pm *PermissionMatrix) SetPermission(p *Permission) {
	if pm.permissions[p.Role] == nil {
		pm.permissions[p.Role] = make(map[string]*Permission)
	}
	pm.permissions[p.Role][p.Doctype] = p
}

// Can checks if a role can perform an operation on a doctype.
func (pm *PermissionMatrix) Can(role, doctype, operation string) bool {
	if role == AdminRole {
		return true // Admin can do everything.
	}
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
	if role == AdminRole {
		return false
	}
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

// AllRoles returns all registered roles.
func (pm *PermissionMatrix) AllRoles() []*Role {
	roles := make([]*Role, 0, len(pm.roles))
	for _, r := range pm.roles {
		roles = append(roles, r)
	}
	return roles
}

// AllPermissions returns all registered permissions.
func (pm *PermissionMatrix) AllPermissions() []*Permission {
	var perms []*Permission
	for _, rolePerms := range pm.permissions {
		for _, p := range rolePerms {
			perms = append(perms, p)
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
	for _, r := range roles {
		pm.AddRole(r)
	}
	for _, p := range permissions {
		pm.SetPermission(p)
	}
}

// ParseRolesFile parses a roles.yaml file.
func ParseRolesFile(path string) ([]*Role, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading roles file: %w", err)
	}
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
