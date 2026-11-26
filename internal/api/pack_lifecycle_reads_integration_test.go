//go:build integration

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
)

// A document-event script reads other records as part of the save the person may make.
// The Clerk may save and read a Read Source but has no permission on Read Target. The
// Source's before_save reads every target with kora.getList and kora.getDoc and records
// what it saw. The Clerk's direct reads of the target are still refused, and an
// API-method script run as the same Clerk still reads nothing.
func TestPackScriptsLifecycleReads(t *testing.T) {
	pack := writeLifecycleReadPack(t)
	eachPackDialect(t, []string{pack}, func(admin *packFixture) {
		first := admin.create("Read Target", `{"label":"first"}`)
		second := admin.create("Read Target", `{"label":"second"}`)
		admin.exec("UPDATE "+admin.quote("tabRead Target")+" SET owner = ? WHERE name = ?", "someone-else@example.test", second)

		admin.run("clerk save reads targets from before_save", func(f *packFixture) {
			source := f.as("Clerk").create("Read Source", jsonBody(f.t, map[string]string{"target": first, "note": "read"}))
			f.expect("Read Source", source, "seen_count", "2")
			f.expect("Read Source", source, "seen_exists", "yes")
		})

		admin.run("clerk direct read of target still refused", func(f *packFixture) {
			roles := []string{"Clerk"}
			list := serveOwnedRead(f.t, f.handler, f.db, f.reg, f.site, f.dialect, "/api/resource/Read%20Target", roles)
			get := serveOwnedRead(f.t, f.handler, f.db, f.reg, f.site, f.dialect, "/api/resource/Read%20Target/"+first, roles)
			for label, rec := range map[string]*httptest.ResponseRecorder{"list": list, "get": get} {
				if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "first") {
					f.t.Fatalf("clerk %s of Read Target = %d, want refused: %s", label, rec.Code, rec.Body.String())
				}
			}
		})

		admin.run("api method script still reads nothing", func(f *packFixture) {
			tx := &orm.TxManager{
				DB: f.db, Registry: f.reg, Dialect: kdb.Resolve(f.dialect), SiteName: f.site,
				Execution: integrationTestExecution(f.t, f.site, "clerk@example.test", []string{"Clerk"}),
			}
			runner := script.NewEmbeddedRunner(script.DefaultEmbeddedConfig())
			f.t.Cleanup(func() { _ = runner.Close() })
			result, err := runner.Execute(context.Background(), script.ExecuteRequest{
				Script:     `return [kora.getList("Read Target", {limit: 10}).length, kora.getDoc("Read Target", "` + first + `") ? "yes" : "no"].join(",");`,
				ScriptType: script.TypeAPIMethod, ScriptName: "read-probe", DocType: "Read Source", Site: f.site,
				User: "clerk@example.test", UserRoles: []string{"Clerk"}, Timeout: 5 * time.Second,
				Provider: NewScriptProvider(tx, f.reg, f.site, nil, nil),
			})
			if err != nil {
				f.t.Fatalf("api method probe: %v", err)
			}
			if got := result.Result; got != "0,no" {
				f.t.Fatalf("api method read = %v, want the Clerk's read denied (\"0,no\")", got)
			}
		})
	})
}

// writeLifecycleReadPack writes the tiny pack this test loads: a Read Source the Clerk
// saves, a Read Target the Clerk may not read, and the source's before_save probe.
func writeLifecycleReadPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"doctypes/read-source.yaml": `name: Read Source
module: Reads
fields:
  - fieldname: target
    fieldtype: Data
    label: Target
  - fieldname: note
    fieldtype: Data
    label: Note
  - fieldname: seen_count
    fieldtype: Data
    label: Seen count
  - fieldname: seen_exists
    fieldtype: Data
    label: Seen exists
`,
		"doctypes/read-target.yaml": `name: Read Target
module: Reads
fields:
  - fieldname: label
    fieldtype: Data
    label: Label
`,
		"roles.yaml": `- name: Clerk
  workspace_access: true
`,
		"permissions.yaml": `- doctype: Read Source
  role: Clerk
  read: true
  write: true
  create: true
  delete: false
`,
		"scripts/Read Source/before_save.js": `var doc = __kora_event__.doc;
var rows = kora.getList("Read Target", {limit: 10});
var one = kora.getDoc("Read Target", doc.target);
doc.seen_count = String(rows.length);
doc.seen_exists = one ? "yes" : "no";
return {doc: doc};
`,
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
