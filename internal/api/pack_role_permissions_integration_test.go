//go:build integration

package api

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// The packs added for the niche templates. The pack tests run as Administrator, so this is the one place the
// narrower roles are exercised: loaded together into one MySQL (which also shows the packs coexist), every role
// is refused exactly what its permission rows withhold, and nothing more.
var rolePermissionPacks = []string{
	"crm", "people", "documents", "assets", "projects", "fleet", "manufacturing", "subscriptions",
	"menu-tables", "batch-expiry", "farm-produce", "students", "shipments", "properties", "appointments",
	"job-cards", "members-savings", "grants-donors", "rooms-bookings",
}

type rolePerm struct{ read, create, write, delete bool }

func TestNicheRolesGetExactlyTheirDeclaredPermissions(t *testing.T) {
	var roles []*doctype.Role
	var permissions []*doctype.Permission
	recordTypes := map[string][]string{} // role -> the record types of its pack
	appendOnly := map[string]bool{}      // record types that refuse every edit and delete
	seen := map[string]bool{}
	for _, pack := range rolePermissionPacks {
		root := filepath.Join("..", "..", "config", pack)
		packRoles, packPermissions, err := doctype.ParseRolesDirectory(root)
		if err != nil {
			t.Fatalf("%s roles: %v", pack, err)
		}
		types, err := doctype.ParseConfigTree(root)
		if err != nil {
			t.Fatalf("%s record types: %v", pack, err)
		}
		for _, role := range packRoles {
			if role.Name == doctype.AdminRole || seen[role.Name] {
				continue
			}
			seen[role.Name] = true
			roles = append(roles, role)
			for _, dt := range types {
				if !dt.IsChildTable {
					recordTypes[role.Name] = append(recordTypes[role.Name], dt.Name)
					appendOnly[dt.Name] = dt.AppendOnly
				}
			}
		}
		permissions = append(permissions, packPermissions...)
	}

	handler, db, reg, site := newPackLayersHandlerWithRoles(t, roles, permissions, append(append([]string{}, rolePermissionPacks...), "pos-base")...)

	declared := map[string]map[string]rolePerm{} // role -> record type -> what it may do
	for _, p := range permissions {
		if declared[p.Role] == nil {
			declared[p.Role] = map[string]rolePerm{}
		}
		cur := declared[p.Role][p.Doctype]
		declared[p.Role][p.Doctype] = rolePerm{cur.read || p.Read, cur.create || p.Create, cur.write || p.Write, cur.delete || p.Delete}
	}
	forbidden := func(code int) bool { return code == http.StatusForbidden || code == http.StatusUnauthorized }

	checked := 0
	for _, role := range roles {
		for _, recordType := range recordTypes[role.Name] {
			want := declared[role.Name][recordType]
			as := []string{role.Name}
			escaped := url.PathEscape(recordType)
			name := role.Name + " on " + recordType

			got := serveOwnedRead(t, handler, db, reg, site, "mysql", "/api/resource/"+escaped, as).Code
			if forbidden(got) == want.read {
				t.Errorf("%s: read answered %d, but the permission file says read=%v", name, got, want.read)
			}
			// An empty body is never a valid record, so a role that may create gets a validation refusal and one that may
			// not gets a permission refusal; the two are told apart by the status.
			got = serveResourceMutationBody(t, handler, db, reg, site, httpMethodPost, "/api/resource/"+escaped, `{}`, "", as).Code
			if forbidden(got) == want.create {
				t.Errorf("%s: create answered %d, but the permission file says create=%v", name, got, want.create)
			}
			got = serveResourceMutationBody(t, handler, db, reg, site, httpMethodPut, "/api/resource/"+escaped+"/NO-SUCH-RECORD", `{}`, "", as).Code
			wrote := got
			got = serveResourceMutationBody(t, handler, db, reg, site, httpMethodDelete, "/api/resource/"+escaped+"/NO-SUCH-RECORD", ``, "", as).Code
			if appendOnly[recordType] {
				// The append-only rule refuses every edit and delete before permissions are consulted, so what matters
				// is that nothing is accepted, whatever the role.
				if wrote < 400 || got < 400 {
					t.Errorf("%s: an append-only record type accepted an edit (%d) or a delete (%d)", name, wrote, got)
				}
			} else {
				if forbidden(wrote) == want.write {
					t.Errorf("%s: write answered %d, but the permission file says write=%v", name, wrote, want.write)
				}
				if forbidden(got) == want.delete {
					t.Errorf("%s: delete answered %d, but the permission file says delete=%v", name, got, want.delete)
				}
			}
			checked++
		}
	}
	if checked < 50 {
		t.Fatalf("only %d role and record type pairs were checked", checked)
	}
	t.Logf("checked %d role and record type pairs across %d roles", checked, len(roles))
}
