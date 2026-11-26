package script

import (
	"context"
	"strings"
	"testing"
)

func TestLibrariesLoadBeforeScript(t *testing.T) {
	tests := []struct {
		name      string
		libraries []Library
		script    string
		want      any
		wantErr   string
	}{
		{
			name:      "function declared by a library is callable from the script",
			libraries: []Library{{Name: "lib_util", Source: `function twice(n) { return n * 2; }`}},
			script:    `return {result: twice(21)};`,
			want:      int64(42),
		},
		{
			name:      "variable declared by a library is readable from the script",
			libraries: []Library{{Name: "lib_const", Source: `var LIMIT = 7;`}},
			script:    `return {result: LIMIT};`,
			want:      int64(7),
		},
		{
			name: "libraries load in the given order",
			libraries: []Library{
				{Name: "lib_a", Source: `var base = 1;`},
				{Name: "lib_b", Source: `var next = base + 1;`},
			},
			script: `return {result: next};`,
			want:   int64(2),
		},
		{
			name:      "a library that throws fails the run and names it",
			libraries: []Library{{Name: "lib_broken", Source: `throw new Error("bad table");`}},
			script:    `return {result: 1};`,
			wantErr:   `library "lib_broken" failed to load`,
		},
		{
			name:   "no libraries leaves the script unchanged",
			script: `return {result: "plain"};`,
			want:   "plain",
		},
	}
	runner := NewEmbeddedRunner(DefaultEmbeddedConfig())
	t.Cleanup(func() { _ = runner.Close() }) // test-only runner; Close errors cannot change the assertions
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runner.Execute(context.Background(), ExecuteRequest{
				Script: tc.script, Libraries: tc.libraries,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "bad table") {
					t.Fatalf("Execute error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Result != tc.want {
				t.Fatalf("result = %v, want %v", result.Result, tc.want)
			}
		})
	}
}
