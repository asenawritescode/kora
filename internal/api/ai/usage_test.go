package ai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/contract"
)

func TestRecordAudit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	ctx := enrichAuditContext(context.Background(), "user-1", "sid-1", "corr-1", "idem-1")
	mock.ExpectExec("INSERT INTO _kora_ai_audit").
		WithArgs(
			sqlmock.AnyArg(),
			"site-a",
			"run-1",
			"step-1",
			"conv-1",
			"model_attempt",
			"gpt-4o",
			"completed",
			"user-1",
			"sid-1",
			"corr-1",
			"idem-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := RecordAudit(ctx, db, AuditEvent{
		Site:           "site-a",
		RunID:          "run-1",
		StepID:         "step-1",
		ConversationID: "conv-1",
		Kind:           "model_attempt",
		Name:           "gpt-4o",
		Status:         "completed",
		Details:        map[string]any{"round": 1},
	}); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// decodeUsageFixture reads a provider response fixture, the way callAI decodes a completion.
func decodeUsageFixture(t *testing.T, raw string) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return resp
}

func TestParseUsageAcrossProviders(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    Usage
	}{
		{
			name:    "openai reports cached prompt tokens in details",
			fixture: `{"usage":{"prompt_tokens":1200,"completion_tokens":80,"total_tokens":1280,"prompt_tokens_details":{"cached_tokens":1024}}}`,
			want:    Usage{Prompt: 1200, Completion: 80, Total: 1280, CachedInput: 1024},
		},
		{
			name:    "deepseek reports cache hits and misses",
			fixture: `{"usage":{"prompt_tokens":900,"completion_tokens":40,"total_tokens":940,"prompt_cache_hit_tokens":640,"prompt_cache_miss_tokens":260}}`,
			want:    Usage{Prompt: 900, Completion: 40, Total: 940, CachedInput: 640},
		},
		{
			name:    "anthropic adds cache reads and writes back to input",
			fixture: `{"usage":{"input_tokens":50,"output_tokens":30,"cache_read_input_tokens":700,"cache_creation_input_tokens":200}}`,
			want:    Usage{Prompt: 950, Completion: 30, Total: 980, CachedInput: 700, CacheWrite: 200},
		},
		{
			name:    "no usage object is zero",
			fixture: `{"choices":[]}`,
			want:    Usage{},
		},
		{
			name:    "a non-numeric count is zero",
			fixture: `{"usage":{"prompt_tokens":"many","completion_tokens":5}}`,
			want:    Usage{Completion: 5, Total: 5},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseUsage(decodeUsageFixture(t, tc.fixture)); got != tc.want {
				t.Errorf("parseUsage() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUsageTokenClassesAndRunSum(t *testing.T) {
	round := Usage{Prompt: 100, Completion: 10, Total: 110, CachedInput: 60}
	run := round.add(Usage{Prompt: 40, Completion: 5, Total: 45, CacheWrite: 8})
	if run != (Usage{Prompt: 140, Completion: 15, Total: 155, CachedInput: 60, CacheWrite: 8}) {
		t.Fatalf("run sum = %+v", run)
	}
	classes := run.tokenClasses()
	want := map[contract.UsageClass]int64{
		contract.UsageClassInput:       140,
		contract.UsageClassOutput:      15,
		contract.UsageClassTotal:       155,
		contract.UsageClassCachedInput: 60,
		contract.UsageClassCacheWrite:  8,
	}
	if len(classes) != len(want) {
		t.Fatalf("token classes = %v, want %v", classes, want)
	}
	for class, n := range want {
		if classes[class] != n {
			t.Errorf("class %s = %d, want %d", class, classes[class], n)
		}
	}
	if got := (Usage{}).tokenClasses(); len(got) != 0 {
		t.Errorf("zero usage stored classes %v, want none", got)
	}
}
