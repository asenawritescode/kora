//go:build integration
// +build integration

package kernel_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/kernel"
)

const ownTaskYAML = `
name: task.touch
namespace: planning
version: 1
input:
  record: Task
require:
  - exists:
      record: Task
      where:
        name: $input.task
        title: $actor
    unless_role: [Writer]
    message: Task belongs to someone else
transaction:
  - update:
      record: Task
      name: $input.task
      values:
        status: Done
`

// TestUnlessRoleGuardThroughTheKernel runs a guard on "owned by the actor"
// that Writer skips. Task.title is the owner marker so no extra doctype is
// needed. Exact role matching and the no-roles case are covered by the
// skippedFor unit test, since the permission matrix rejects such actors first.
func TestUnlessRoleGuardThroughTheKernel(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, ownTaskYAML)

	mustComplete(t, exec(t, s, k, createOp(map[string]any{"title": "alice", "serial": "S-1"})))
	var task string
	if err := s.DB.QueryRow("SELECT name FROM `tabTask` WHERE serial='S-1'").Scan(&task); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		user  string
		roles []string
		allow bool
	}{
		{"own record, guard satisfied", "alice", []string{"Administrator"}, true},
		{"someone else's, unlisted role", "bob", []string{"Administrator"}, false},
		{"someone else's, listed role", "bob", []string{"Writer"}, true},
		{"someone else's, listed role among several", "bob", []string{"Administrator", "Writer"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.DB.Exec("UPDATE `tabTask` SET status='Open' WHERE name='T-1'"); err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]any{"data": map[string]any{"task": task}})
			op := kernel.Operation{Command: "planning.task.touch", Payload: payload, Context: kernel.OperationContext{User: tc.user, Roles: tc.roles}}
			result := exec(t, s, k, op)
			if tc.allow {
				mustComplete(t, result)
				return
			}
			if result.Status == contract.StatusCompleted || result.Error == nil || !strings.Contains(result.Error.Message, "belongs to someone else") {
				t.Fatalf("result = %+v, want refusal with the guard message", result)
			}
		})
	}
}
