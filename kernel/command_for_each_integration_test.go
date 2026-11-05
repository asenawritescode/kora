//go:build integration
// +build integration

package kernel_test

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/kernel"
)

const taskBatchYAML = `
name: task.plan
namespace: planning
version: 1
input:
  record: Task
transaction:
  - as: parent
    create:
      record: Task
      values:
        title: $input.title
  - for_each: $input.subtasks
    create:
      record: Task
      values:
        title: $item.title
        related_task: $steps.parent.name
`

// TestConfiguredCommandForEachWritesParentAndChildrenAtomically proves one
// configured command writes a parent and one linked record per list element
// in a single transaction.
func TestConfiguredCommandForEachWritesParentAndChildrenAtomically(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, taskBatchYAML)

	payload, _ := json.Marshal(map[string]any{"data": map[string]any{
		"title": "Launch", "subtasks": []any{map[string]any{"title": "Print"}, map[string]any{"title": "Ship"}},
	}})
	res := exec(t, s, k, kernel.Operation{Command: "planning.task.plan", Payload: payload, Context: opCtx(s.Name, "Administrator")})
	mustComplete(t, res)

	var parent string
	s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title='Launch'").Scan(&parent)
	if parent == "" {
		t.Fatal("parent was not created")
	}
	if n := s.count(t, "SELECT COUNT(*) FROM `tabTask` WHERE related_task=?", parent); n != 2 {
		t.Fatalf("linked children = %d, want 2", n)
	}

	bad, _ := json.Marshal(map[string]any{"data": map[string]any{
		"title": "Rollback", "subtasks": []any{map[string]any{"title": "Ok"}, map[string]any{"title": ""}},
	}})
	failed := exec(t, s, k, kernel.Operation{Command: "planning.task.plan", Payload: bad, Context: opCtx(s.Name, "Administrator")})
	if failed.Status == "completed" {
		t.Fatal("command with an invalid element completed")
	}
	if n := s.count(t, "SELECT COUNT(*) FROM `tabTask` WHERE title IN ('Rollback','Ok')"); n != 0 {
		t.Fatalf("partial writes survived a failed element: %d", n)
	}
}
