//go:build integration
// +build integration

package kernel_test

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/kernel"
)

const openTaskYAML = `
name: task.open
namespace: planning
version: 1
input:
  record: Task
require:
  - lock:
      record: Task
      name: $input.board
  - not_exists:
      record: Task
      where:
        title: $input.title
        status: Open
    message: An open task with this title already exists
transaction:
  - create:
      record: Task
      values:
        title: $input.title
        status: Open
        related_task: $input.board
`

const closeTaskYAML = `
name: task.close
namespace: planning
version: 1
input:
  record: Task
require:
  - exists:
      record: Task
      where:
        title: $input.title
        status: Open
    as: open_task
    message: No open task with this title
transaction:
  - update:
      record: Task
      name: $steps.open_task.name
      values:
        status: Done
`

func TestConfiguredCommandGuardsEnforceOneOpenRecordUnderConcurrency(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, openTaskYAML)
	mustRegister(t, k, closeTaskYAML)

	board := exec(t, s, k, createOp(map[string]any{"title": "Board", "serial": "BOARD-1"}))
	mustComplete(t, board)
	var boardName string
	s.DB.QueryRow("SELECT name FROM `tabTask` WHERE serial='BOARD-1'").Scan(&boardName)

	run := func(command string, data map[string]any) contract.CommandResult {
		payload, _ := json.Marshal(map[string]any{"data": data})
		return exec(t, s, k, kernel.Operation{Command: command, Payload: payload, Context: opCtx(s.Name, "Administrator")})
	}

	var wg sync.WaitGroup
	results := make([]contract.CommandResult, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = run("planning.task.open", map[string]any{"title": "Till", "board": boardName})
		}(i)
	}
	wg.Wait()
	completed := 0
	for _, res := range results {
		if res.Status == contract.StatusCompleted {
			completed++
		} else if res.Error == nil || res.Error.Type != contract.CodeConflict {
			t.Fatalf("unexpected failure: %#v", res.Error)
		}
	}
	if completed != 1 {
		t.Fatalf("completed opens = %d, want 1", completed)
	}

	mustComplete(t, run("planning.task.close", map[string]any{"title": "Till"}))
	if n := s.count(t, "SELECT COUNT(*) FROM `tabTask` WHERE title='Till' AND status='Done'"); n != 1 {
		t.Fatalf("close did not update the bound record")
	}
	closed := run("planning.task.close", map[string]any{"title": "Till"})
	if closed.Status == contract.StatusCompleted || closed.Error == nil || closed.Error.Type != contract.CodeConflict {
		t.Fatalf("close without an open record = %#v", closed)
	}
	missingLock := run("planning.task.open", map[string]any{"title": "Other", "board": "TASK-9999"})
	if missingLock.Error == nil || missingLock.Error.Type != contract.CodeNotFound {
		t.Fatalf("lock on a missing record = %#v", missingLock)
	}
}
