//go:build liveeval

package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/secret"
)

// TestLiveRecordTaskEvals is the H6b harness: the ten record tasks, two runs each, through HandleChat on a real
// database. Each task runs on a fresh tenant. A write task that opens a preview is answered with "yes" once, and
// every task is scored from its SQL state and its reply.
//
// Environment (the planner runs it; CI never does):
//
//	KORA_LIVE_EVAL_REPORT            Markdown report path (required)
//	KORA_LIVE_EVAL_MYSQL_DSN         disposable MySQL DSN; the MySQL run happens when set
//	KORA_LIVE_EVAL_POSTGRES_DSN      disposable Postgres DSN; the Postgres run happens when set
//	KORA_LIVE_EVAL_DEEPSEEK_API_KEY  DeepSeek key, read from env only and never logged or written to the report
//	KORA_LIVE_EVAL_FAKE=1            replay the scripted turns of TestRecordTaskEvals through a local fake that speaks
//	                                 the OpenAI chat-completions format; no key and no network are used
//
// The provider is DeepSeek (model deepseek-flash, thinking off by the provider's default). The fake stands in for
// api.deepseek.com through the test's http.DefaultTransport, so the production code path is unchanged.
func TestLiveRecordTaskEvals(t *testing.T) {
	reportPath := strings.TrimSpace(os.Getenv("KORA_LIVE_EVAL_REPORT"))
	if reportPath == "" {
		t.Skip("set KORA_LIVE_EVAL_REPORT to the Markdown report path")
	}
	fake := os.Getenv("KORA_LIVE_EVAL_FAKE") == "1"
	key := os.Getenv("KORA_LIVE_EVAL_DEEPSEEK_API_KEY")
	if fake {
		key = "fake-key"
	}
	if key == "" {
		t.Skip("set KORA_LIVE_EVAL_DEEPSEEK_API_KEY, or KORA_LIVE_EVAL_FAKE=1 for the fake provider")
	}
	var report []liveDialectRun
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_LIVE_EVAL_MYSQL_DSN"},
		{"postgres", "KORA_LIVE_EVAL_POSTGRES_DSN"},
	} {
		dsn := strings.TrimSpace(os.Getenv(tc.env))
		if dsn == "" {
			continue
		}
		dialect := liveDialectRun{driver: tc.driver}
		f := newChatFixture(t, tc.driver, dsn)
		if fake {
			useFakeDeepSeek(t, f)
		} else {
			f.useDeepSeek(key)
		}
		for _, task := range recordTasks {
			for run := 1; run <= 2; run++ {
				dialect.runs = append(dialect.runs, runLiveRecordTask(t, f, task, run, fake))
			}
		}
		report = append(report, dialect)
	}
	if len(report) == 0 {
		t.Skip("set KORA_LIVE_EVAL_MYSQL_DSN or KORA_LIVE_EVAL_POSTGRES_DSN")
	}
	if err := writeLiveEvalReport(reportPath, report, fake); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("wrote %s", reportPath)
}

// liveOutcome classes one run: pass when the SQL state and the reply rule both hold, partial when one holds, and
// fail when neither does.
type liveOutcome string

const (
	liveOutcomePass    liveOutcome = "pass"
	liveOutcomePartial liveOutcome = "partial"
	liveOutcomeFail    liveOutcome = "fail"
)

// liveRun is the score of one run of one task.
type liveRun struct {
	task       recordTask
	run        int
	outcome    liveOutcome
	settled    bool
	previews   int
	calls      int
	input      int64
	cached     int64
	output     int64
	replyChars int
	latency    time.Duration
	err        string
}

// liveDialectRun is every run of one dialect.
type liveDialectRun struct {
	driver string
	runs   []liveRun
}

// runLiveRecordTask runs one task once on the dialect's tenant, from a reset, and scores it.
func runLiveRecordTask(t *testing.T, f *chatFixture, task recordTask, run int, fake bool) liveRun {
	t.Helper()
	f.t = t
	f.reset(seedRecordTaskBase)
	var scripted []scriptedRecordTurn
	if fake {
		scripted = recordTaskScripts[task.id](f)
	}
	result := liveRun{task: task, run: run}
	messages := []string{task.message}
	var reply string
	start := time.Now()
	for i := 0; i < len(messages); i++ {
		if fake {
			if i >= len(scripted) {
				result.err = fmt.Sprintf("the fake script has %d turns; the run sent turn %d", len(scripted), i+1)
				break
			}
			f.model.script(scripted[i].steps(f))
		}
		code, raw := f.post(messages[i])
		if code != http.StatusOK {
			result.err = fmt.Sprintf("turn %d answered %d", i+1, code)
			break
		}
		reply = chatReply(raw)
		if i == 0 && task.writes && f.openPreviews() > 0 {
			messages = append(messages, "yes")
		}
	}
	result.latency = time.Since(start)
	result.previews = f.openPreviews()
	result.replyChars = len([]rune(reply))
	result.settled = task.settled(f) == nil
	answered := task.answered == nil || task.answered(reply)
	result.outcome = liveOutcomeOf(result.settled, answered)
	result.calls, result.input, result.cached, result.output = f.usageTotals()
	return result
}

// liveOutcomeOf classes a run from its two checks.
func liveOutcomeOf(settled, answered bool) liveOutcome {
	switch {
	case settled && answered:
		return liveOutcomePass
	case settled || answered:
		return liveOutcomePartial
	default:
		return liveOutcomeFail
	}
}

// useDeepSeek stores a DeepSeek key for the site and clears the Cloud gateway, so the chat calls DeepSeek directly.
func (f *chatFixture) useDeepSeek(key string) {
	f.t.Setenv("KORA_CLOUD_AI_GATEWAY_URL", "")
	if err := secret.NewStore(f.database, f.dialect).Set(f.siteName, "deepseek_api_key", key); err != nil {
		f.t.Fatalf("store the provider key: %v", err)
	}
}

// usageTotals sums the stored provider usage of the site: model calls, input, cached input and output tokens.
func (f *chatFixture) usageTotals() (calls int, input, cached, output int64) {
	f.t.Helper()
	rows, err := f.database.Query(db.Rebind(f.dialect, "SELECT tokens FROM _kora_ai_usage WHERE site = ?"), f.siteName)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			f.t.Fatal(err)
		}
		var tokens map[string]int64
		_ = json.Unmarshal([]byte(raw), &tokens)
		calls++
		input += tokens["input"]
		cached += tokens["cached_input"]
		output += tokens["output"]
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return calls, input, cached, output
}

// useFakeDeepSeek routes the chat's DeepSeek calls to the fake's server for the rest of the test.
func useFakeDeepSeek(t *testing.T, f *chatFixture) {
	t.Helper()
	target, err := url.Parse(f.model.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = deepSeekRedirect{base: previous, target: target}
	t.Cleanup(func() { http.DefaultTransport = previous })
	f.useDeepSeek("fake-key")
}

// deepSeekRedirect sends requests for api.deepseek.com to the fake server and leaves every other request alone.
type deepSeekRedirect struct {
	base   http.RoundTripper
	target *url.URL
}

func (r deepSeekRedirect) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "api.deepseek.com" {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = r.target.Scheme
		clone.URL.Host = r.target.Host
		clone.Host = r.target.Host
		return r.base.RoundTrip(clone)
	}
	return r.base.RoundTrip(req)
}

// writeLiveEvalReport writes the Markdown report. It holds no key, DSN or reply text.
func writeLiveEvalReport(path string, dialects []liveDialectRun, fake bool) error {
	var b strings.Builder
	mode := "live (DeepSeek API, model deepseek-flash, thinking off)"
	if fake {
		mode = "fake replay (scripted OpenAI chat-completions fake; no provider call)"
	}
	fmt.Fprintf(&b, "# Record-task evals (H6b)\n\nGenerated: %s\n\nMode: %s\n\n", time.Now().UTC().Format(time.RFC3339), mode)
	for _, d := range dialects {
		fmt.Fprintf(&b, "## %s\n\n", d.driver)
		b.WriteString("| Task | Run | Outcome | Write verified | Open previews | Model calls | Input tokens | Cached input | Output tokens | Reply chars | Latency ms | Note |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, r := range d.runs {
			fmt.Fprintf(&b, "| %d | %d | %s | %s | %d | %d | %d | %d | %d | %d | %d | %s |\n",
				r.task.id, r.run, r.outcome, yesNo(r.settled && r.task.writes, r.task.writes), r.previews, r.calls,
				r.input, r.cached, r.output, r.replyChars, r.latency.Milliseconds(), r.err)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "### Consistency and verdict\n\n")
		passing := [2]int{}
		consistent := 0
		for _, task := range recordTasks {
			first, second := d.outcomeOf(task.id, 1), d.outcomeOf(task.id, 2)
			if first == second {
				consistent++
			}
			for i, o := range [2]liveOutcome{first, second} {
				if o == liveOutcomePass {
					passing[i]++
				}
			}
		}
		fmt.Fprintf(&b, "- Tasks passing: run 1 %d/10, run 2 %d/10. Target is at least 9/10 in both runs.\n", passing[0], passing[1])
		fmt.Fprintf(&b, "- Same outcome class in both runs: %d/10 tasks.\n", consistent)
		verdict := "NOT MET"
		if passing[0] >= 9 && passing[1] >= 9 && consistent == len(recordTasks) {
			verdict = "MET"
		}
		fmt.Fprintf(&b, "- Target: %s.\n\n", verdict)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// outcomeOf returns the outcome of one run of one task in this dialect.
func (d liveDialectRun) outcomeOf(id, run int) liveOutcome {
	for _, r := range d.runs {
		if r.task.id == id && r.run == run {
			return r.outcome
		}
	}
	return liveOutcomeFail
}

func yesNo(yes, applicable bool) string {
	switch {
	case !applicable:
		return "n/a"
	case yes:
		return "yes"
	default:
		return "no"
	}
}
