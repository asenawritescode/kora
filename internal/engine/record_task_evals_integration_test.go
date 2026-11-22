//go:build integration

package engine

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestRecordTaskEvals runs the ten H0 record tasks through the chat on the POS pack, with a scripted model that
// makes the same tool calls a correct model would. It asserts the harness side of each task: the preview comes
// first, nothing is written before the person's yes, the open preview is listed in the next turn's context, the
// error codes of the refused calls, and the SQL state the finished task leaves. The live twin is
// TestLiveRecordTaskEvals (build tag liveeval).
func TestRecordTaskEvals(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_RECORD_TASK_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_RECORD_TASK_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			// One tenant per dialect. Each case resets its business rows and starts its own conversation.
			f := newChatFixture(t, tc.driver, dsn)
			f.useScriptedGateway()
			for _, task := range recordTasks {
				t.Run(fmt.Sprintf("task-%02d", task.id), func(t *testing.T) {
					f.t = t
					f.reset(seedRecordTaskBase)
					runScriptedRecordTask(t, f, task, recordTaskScripts[task.id](f))
				})
			}
		})
	}
}

// runScriptedRecordTask plays the scripted turns of one task and asserts its harness result.
func runScriptedRecordTask(t *testing.T, f *chatFixture, task recordTask, turns []scriptedRecordTurn) {
	t.Helper()
	if len(turns) == 0 || turns[0].message != task.message {
		t.Fatalf("task %d: the script does not start with the task message", task.id)
	}
	var reply string
	for i, turn := range turns {
		bodies, got := f.chatScripted(turn.message, turn.steps(f))
		if turn.check != nil {
			if err := turn.check(f, bodies); err != nil {
				t.Fatalf("task %d turn %d: %v", task.id, i+1, err)
			}
		}
		if task.writes && i == 1 && !strings.Contains(bodies[0], "OPEN PREVIEWS") {
			t.Fatalf("task %d: the turn after the preview does not list the open preview", task.id)
		}
		reply = got
	}
	if task.writes && f.openPreviews() != 0 {
		t.Fatalf("task %d: a preview is still open after the yes turn", task.id)
	}
	if err := task.settled(f); err != nil {
		t.Fatalf("task %d: %v", task.id, err)
	}
	if task.answered != nil && !task.answered(reply) {
		t.Fatalf("task %d: reply %q does not meet the reply rule", task.id, reply)
	}
}
