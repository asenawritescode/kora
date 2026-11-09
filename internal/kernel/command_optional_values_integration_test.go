//go:build integration
// +build integration

package kernel_test

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/kernel"
)

const optionalYAML = `
name: task.make
namespace: planning
version: 1
input:
  record: Task
transaction:
  - create:
      record: Task
      values:
        title: $input.title
        related_task: $input.related
`

func TestCommandValuesOmitAbsentInputRefs(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, optionalYAML)
	run := func(data map[string]any) contract.CommandResult {
		p, _ := json.Marshal(map[string]any{"data": data})
		return exec(t, s, k, kernel.Operation{Command: "planning.task.make", Payload: p, Context: opCtx(s.Name, "Administrator")})
	}

	mustComplete(t, run(map[string]any{"title": "NoRel"}))
	if n := s.count(t, "SELECT COUNT(*) FROM `tabTask` WHERE title='NoRel' AND related_task IS NULL"); n != 1 {
		t.Fatalf("absent optional ref should leave field empty, rows=%d", n)
	}
	mustComplete(t, run(map[string]any{"title": "Nulled", "related": nil}))

	res := run(map[string]any{"related": ""})
	if res.Status == contract.StatusCompleted || res.Error == nil {
		t.Fatal("absent required title must fail")
	}
	if res.Error.Message == "" || contains(res.Error.Message, "missing in input") {
		t.Fatalf("want field validation error, got %q", res.Error.Message)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
