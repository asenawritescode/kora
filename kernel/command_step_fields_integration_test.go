//go:build integration
// +build integration

package kernel_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/kernel"
)

const cloneTaskYAML = `
name: task.clone
namespace: planning
version: 1
input:
  record: Task
require:
  - exists:
      record: Task
      where:
        title: $input.source
    as: src
transaction:
  - as: copy
    create:
      record: Task
      values:
        title: $input.title
        priority: $steps.src.priority
        status: Open
  - update:
      record: Task
      name: $steps.copy.name
      values:
        related_task: $steps.src.name
        serial: $steps.copy.title
output:
  copied_priority: $steps.copy.priority
  source_status: $steps.src.status
`

func cloneOp(title, source string, role string) kernel.Operation {
	payload, _ := json.Marshal(map[string]any{"data": map[string]any{"title": title, "source": source}})
	return kernel.Operation{Command: "planning.task.clone", Payload: payload, Context: opCtx("site-a", role)}
}

// TestConfiguredCommandStepsReadFieldsOfEarlierRecords proves a later step can
// copy any scalar field from a require alias or from a record an earlier step
// wrote, and that the same references work in outputs.
func TestConfiguredCommandStepsReadFieldsOfEarlierRecords(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, cloneTaskYAML)
	mustComplete(t, exec(t, s, k, createOp(map[string]any{"title": "Source", "priority": "High", "status": "Done"})))

	res := exec(t, s, k, cloneOp("Clone", "Source", "Administrator"))
	mustComplete(t, res)

	var priority, serial, related string
	if err := s.DB.QueryRow("SELECT priority, serial, related_task FROM `tabTask` WHERE title='Clone'").Scan(&priority, &serial, &related); err != nil {
		t.Fatal(err)
	}
	var source string
	s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title='Source'").Scan(&source)
	if priority != "High" || serial != "Clone" || related != source {
		t.Fatalf("priority=%q serial=%q related=%q, want High/Clone/%s", priority, serial, related, source)
	}
	var out struct {
		Output map[string]any `json:"output"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Output["copied_priority"] != "High" || out.Output["source_status"] != "Done" {
		t.Fatalf("output = %v", out.Output)
	}
}

// TestConfiguredCommandStepFieldReadsKeepPermissionSemantics proves a step
// result never exposes fields the actor cannot read, and that unknown or
// non-scalar fields fail the command without writing anything.
func TestConfiguredCommandStepFieldReadsKeepPermissionSemantics(t *testing.T) {
	const bodyYAML = `
version: 1
input:
  record: Task
require:
  - exists:
      record: Task
      where:
        title: $input.source
    as: src
transaction:
  - create:
      record: Task
      values:
        title: $input.title
        priority: REF
`
	command := func(name, ref string) string {
		return "name: " + name + "\nnamespace: planning\n" + strings.Replace(bodyYAML, "REF", ref, 1)
	}
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, command("task.by_name", "$steps.src.name"))
	mustRegister(t, k, command("task.by_field", "$steps.src.priority"))
	mustRegister(t, k, command("task.by_unknown", "$steps.src.nosuchfield"))
	mustRegister(t, k, command("task.by_system", "$steps.src.owner"))
	mustComplete(t, exec(t, s, k, createOp(map[string]any{"title": "Source", "priority": "High"})))

	run := func(cmd, title, role string) contract.CommandResult {
		payload, _ := json.Marshal(map[string]any{"data": map[string]any{"title": title, "source": "Source"}})
		return exec(t, s, k, kernel.Operation{Command: "planning." + cmd, Payload: payload, Context: opCtx("site-a", role)})
	}
	tests := []struct {
		name, cmd, role string
		wantCode        contract.Code
	}{
		{"reader copies a field", "task.by_field", "Writer", ""},
		{"actor without read is denied the field", "task.by_field", "WriteOnly", contract.CodePermissionDenied},
		{"name needs no read grant", "task.by_name", "WriteOnly", ""},
		{"unknown field fails", "task.by_unknown", "Administrator", contract.CodeValidationFailed},
		{"system column is not a field", "task.by_system", "Administrator", contract.CodeValidationFailed},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title := "T" + string(rune('A'+i))
			res := run(tc.cmd, title, tc.role)
			wrote := s.count(t, "SELECT COUNT(*) FROM `tabTask` WHERE title=?", title)
			if tc.wantCode == "" {
				mustComplete(t, res)
				if wrote != 1 {
					t.Fatalf("wrote %d rows, want 1", wrote)
				}
				return
			}
			if res.Error == nil || res.Error.Type != tc.wantCode || wrote != 0 {
				t.Fatalf("status=%s err=%+v wrote=%d, want %s and no rows", res.Status, res.Error, wrote, tc.wantCode)
			}
		})
	}
	var denied int
	s.DB.QueryRow("SELECT COUNT(*) FROM `tabTask` WHERE priority='High' AND title='TB'").Scan(&denied)
	if denied != 0 {
		t.Fatal("denied actor received the field value")
	}
}
