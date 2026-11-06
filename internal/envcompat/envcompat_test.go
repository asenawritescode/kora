package envcompat

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fakeEnv is an in-memory process environment for Promote.
type fakeEnv map[string]string

func (f fakeEnv) environ() []string {
	out := make([]string, 0, len(f))
	for k, v := range f {
		out = append(out, k+"="+v)
	}
	return out
}

func (f fakeEnv) setenv(k, v string) error {
	f[k] = v
	return nil
}

func TestPromote(t *testing.T) {
	tests := []struct {
		name           string
		env            fakeEnv
		want           fakeEnv
		wantDeprecated []string
		wantOverridden []string
	}{
		{
			name: "only KORA set is untouched and reported deprecated",
			env:  fakeEnv{"KORA_DB_HOST": "db1", "KORA_DB_PORT": "3307"},
			want: fakeEnv{"KORA_DB_HOST": "db1", "KORA_DB_PORT": "3307"},
			// sorted
			wantDeprecated: []string{"KORA_DB_HOST", "KORA_DB_PORT"},
		},
		{
			name: "only ZANA set is promoted and nothing is deprecated",
			env:  fakeEnv{"ZANA_DB_HOST": "db2"},
			want: fakeEnv{"ZANA_DB_HOST": "db2", "KORA_DB_HOST": "db2"},
		},
		{
			name:           "both set with different values: ZANA wins and conflict is reported",
			env:            fakeEnv{"ZANA_DB_HOST": "new-host", "KORA_DB_HOST": "old-host"},
			want:           fakeEnv{"ZANA_DB_HOST": "new-host", "KORA_DB_HOST": "new-host"},
			wantOverridden: []string{"ZANA_DB_HOST"},
		},
		{
			name: "both set with equal values is quiet",
			env:  fakeEnv{"ZANA_DB_HOST": "same", "KORA_DB_HOST": "same"},
			want: fakeEnv{"ZANA_DB_HOST": "same", "KORA_DB_HOST": "same"},
		},
		{
			name: "empty ZANA value does not blank a set KORA value",
			env:  fakeEnv{"ZANA_DB_HOST": "", "KORA_DB_HOST": "old-host"},
			want: fakeEnv{"ZANA_DB_HOST": "", "KORA_DB_HOST": "old-host"},
			// KORA is still the effective source, so it is still deprecated.
			wantDeprecated: []string{"KORA_DB_HOST"},
		},
		{
			name: "empty KORA value is overwritten by ZANA without a conflict",
			env:  fakeEnv{"ZANA_DB_HOST": "new-host", "KORA_DB_HOST": ""},
			want: fakeEnv{"ZANA_DB_HOST": "new-host", "KORA_DB_HOST": "new-host"},
		},
		{
			name: "empty KORA value alone is not reported",
			env:  fakeEnv{"KORA_DB_HOST": ""},
			want: fakeEnv{"KORA_DB_HOST": ""},
		},
		{
			name: "empty ZANA value alone is not promoted",
			env:  fakeEnv{"ZANA_DB_HOST": ""},
			want: fakeEnv{"ZANA_DB_HOST": ""},
		},
		{
			name: "names embedding KORA twice map by prefix only",
			env: fakeEnv{
				"ZANA_CLOUD_KORA_RELOAD_URL": "http://new",
				"KORA_CLOUD_KORA_RELOAD_URL": "http://old",
			},
			want: fakeEnv{
				"ZANA_CLOUD_KORA_RELOAD_URL": "http://new",
				"KORA_CLOUD_KORA_RELOAD_URL": "http://new",
			},
			wantOverridden: []string{"ZANA_CLOUD_KORA_RELOAD_URL"},
		},
		{
			name: "ZANA name embedding KORA is promoted to the KORA twin only",
			env:  fakeEnv{"ZANA_CLOUD_KORA_RELOAD_TOKEN": "t"},
			want: fakeEnv{"ZANA_CLOUD_KORA_RELOAD_TOKEN": "t", "KORA_CLOUD_KORA_RELOAD_TOKEN": "t"},
		},
		{
			name: "value containing equals sign survives",
			env:  fakeEnv{"ZANA_DB_PASSWORD": "a=b=c"},
			want: fakeEnv{"ZANA_DB_PASSWORD": "a=b=c", "KORA_DB_PASSWORD": "a=b=c"},
		},
		{
			name: "unrelated and lookalike variables are untouched",
			env: fakeEnv{
				"PATH":          "/bin",
				"HOME":          "/root",
				"MYKORA_X":      "1",
				"XZANA_Y":       "2",
				"zana_lower":    "3",
				"kora_lower":    "4",
				"ZANA_":         "5",
				"KORA_":         "6",
				"ZANAX":         "7",
				"KORAX":         "8",
				"OTEL_SERVICE":  "svc",
				"KORAL_DB_HOST": "9",
			},
			want: fakeEnv{
				"PATH":          "/bin",
				"HOME":          "/root",
				"MYKORA_X":      "1",
				"XZANA_Y":       "2",
				"zana_lower":    "3",
				"kora_lower":    "4",
				"ZANA_":         "5",
				"KORA_":         "6",
				"ZANAX":         "7",
				"KORAX":         "8",
				"OTEL_SERVICE":  "svc",
				"KORAL_DB_HOST": "9",
			},
		},
		{
			name: "mixed deprecated and promoted and conflicting are sorted",
			env: fakeEnv{
				"KORA_B":  "b",
				"KORA_A":  "a",
				"ZANA_C":  "c",
				"KORA_D":  "old",
				"ZANA_D":  "new",
				"PATH":    "/bin",
				"ZANA_AA": "aa",
				"KORA_AA": "x",
			},
			want: fakeEnv{
				"KORA_B":  "b",
				"KORA_A":  "a",
				"ZANA_C":  "c",
				"KORA_C":  "c",
				"KORA_D":  "new",
				"ZANA_D":  "new",
				"PATH":    "/bin",
				"ZANA_AA": "aa",
				"KORA_AA": "aa",
			},
			wantDeprecated: []string{"KORA_A", "KORA_B"},
			wantOverridden: []string{"ZANA_AA", "ZANA_D"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Promote(tc.env.environ(), tc.env.setenv)
			if err != nil {
				t.Fatalf("Promote error = %v", err)
			}
			if !reflect.DeepEqual(tc.env, tc.want) {
				t.Errorf("environment = %v, want %v", tc.env, tc.want)
			}
			if !equalNames(report.Deprecated, tc.wantDeprecated) {
				t.Errorf("Deprecated = %v, want %v", report.Deprecated, tc.wantDeprecated)
			}
			if !equalNames(report.Overridden, tc.wantOverridden) {
				t.Errorf("Overridden = %v, want %v", report.Overridden, tc.wantOverridden)
			}
		})
	}
}

func equalNames(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

func TestPromoteIsIdempotent(t *testing.T) {
	env := fakeEnv{"ZANA_DB_HOST": "new", "KORA_DB_HOST": "old", "KORA_DB_PORT": "1"}
	if _, err := Promote(env.environ(), env.setenv); err != nil {
		t.Fatal(err)
	}
	second, err := Promote(env.environ(), env.setenv)
	if err != nil {
		t.Fatal(err)
	}
	if !equalNames(second.Overridden, nil) {
		t.Errorf("second pass Overridden = %v, want none", second.Overridden)
	}
	if !equalNames(second.Deprecated, []string{"KORA_DB_PORT"}) {
		t.Errorf("second pass Deprecated = %v", second.Deprecated)
	}
}

func TestPromoteSetenvFailure(t *testing.T) {
	boom := errors.New("boom")
	env := fakeEnv{"ZANA_A": "1", "ZANA_B": "2"}
	_, err := Promote(env.environ(), func(string, string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
}

func TestLogReport(t *testing.T) {
	const (
		secretNew = "s3cret-NEW-value"
		secretOld = "s3cret-OLD-value"
		secretKey = "sk-only-kora-secret"
	)
	tests := []struct {
		name       string
		env        fakeEnv
		wantLines  int
		wantInLog  []string
		wantAbsent []string
	}{
		{
			name:       "nothing to report logs nothing",
			env:        fakeEnv{"ZANA_DB_HOST": "h", "PATH": "/bin"},
			wantLines:  0,
			wantAbsent: []string{"KORA", "ZANA"},
		},
		{
			name: "deprecated names appear on exactly one line without values",
			env: fakeEnv{
				"KORA_DB_PASSWORD":           secretKey,
				"KORA_DB_HOST":               "db",
				"KORA_CLOUD_KORA_RELOAD_URL": "http://reload",
			},
			wantLines:  1,
			wantInLog:  []string{"KORA_DB_PASSWORD", "KORA_DB_HOST", "KORA_CLOUD_KORA_RELOAD_URL", "ZANA_"},
			wantAbsent: []string{secretKey, "http://reload", "=db"},
		},
		{
			name: "conflict names the variable but never either value",
			env: fakeEnv{
				"ZANA_DB_PASSWORD": secretNew,
				"KORA_DB_PASSWORD": secretOld,
			},
			wantLines:  1,
			wantInLog:  []string{"ZANA_DB_PASSWORD"},
			wantAbsent: []string{secretNew, secretOld},
		},
		{
			name: "conflict and deprecation are two lines total",
			env: fakeEnv{
				"ZANA_DB_PASSWORD": secretNew,
				"KORA_DB_PASSWORD": secretOld,
				"KORA_DB_HOST":     "h",
				"KORA_DB_USER":     "u",
			},
			wantLines:  2,
			wantInLog:  []string{"ZANA_DB_PASSWORD", "KORA_DB_HOST", "KORA_DB_USER"},
			wantAbsent: []string{secretNew, secretOld},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			report, err := Promote(tc.env.environ(), tc.env.setenv)
			if err != nil {
				t.Fatal(err)
			}
			report.Log(logger)
			out := buf.String()
			lines := 0
			if strings.TrimSpace(out) != "" {
				lines = len(strings.Split(strings.TrimSpace(out), "\n"))
			}
			if lines != tc.wantLines {
				t.Errorf("log lines = %d, want %d:\n%s", lines, tc.wantLines, out)
			}
			for _, s := range tc.wantInLog {
				if !strings.Contains(out, s) {
					t.Errorf("log missing %q:\n%s", s, out)
				}
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(out, s) {
					t.Errorf("log must not contain %q:\n%s", s, out)
				}
			}
		})
	}
}

func TestApplyUsesProcessEnvironment(t *testing.T) {
	t.Setenv("ZANA_ENVCOMPAT_PROBE", "from-zana")
	t.Setenv("KORA_ENVCOMPAT_ONLY", "legacy")
	var buf bytes.Buffer
	report, err := Apply(slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("KORA_ENVCOMPAT_PROBE"); got != "from-zana" {
		t.Errorf("KORA_ENVCOMPAT_PROBE = %q, want from-zana", got)
	}
	found := false
	for _, n := range report.Deprecated {
		if n == "KORA_ENVCOMPAT_ONLY" {
			found = true
		}
	}
	if !found {
		t.Errorf("KORA_ENVCOMPAT_ONLY not reported deprecated: %v", report.Deprecated)
	}
	if strings.Contains(buf.String(), "legacy") || strings.Contains(buf.String(), "from-zana") {
		t.Errorf("log leaked a value: %s", buf.String())
	}
}
