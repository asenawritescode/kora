package dbaccess

import (
	"strings"
	"testing"
	"time"
)

func TestTuneDSN(t *testing.T) {
	tests := []struct {
		name    string
		driver  string
		dsn     string
		timeout time.Duration
		want    []string
		notWant []string
	}{
		{
			name:    "mysql request path adds interpolation charset and execution limit",
			driver:  "mysql",
			dsn:     "root:pw@tcp(127.0.0.1:3306)/app?parseTime=true",
			timeout: 30 * time.Second,
			want:    []string{"interpolateParams=true", "charset=utf8mb4", "max_execution_time=30000"},
		},
		{
			name:    "mysql keeps caller interpolation and charset",
			driver:  "mysql",
			dsn:     "root:pw@tcp(127.0.0.1:3306)/app?interpolateParams=false&charset=utf8mb3",
			timeout: 30 * time.Second,
			want:    []string{"charset=utf8mb3", "max_execution_time=30000"},
			notWant: []string{"interpolateParams=true", "charset=utf8mb4"},
		},
		{
			name:    "mysql keeps caller max_execution_time",
			driver:  "mysql",
			dsn:     "root:pw@tcp(127.0.0.1:3306)/app?max_execution_time=5000",
			timeout: 30 * time.Second,
			want:    []string{"max_execution_time=5000"},
			notWant: []string{"max_execution_time=30000"},
		},
		{
			name:    "mysql maintenance has no execution limit",
			driver:  "mysql",
			dsn:     "root:pw@tcp(127.0.0.1:3306)/",
			timeout: 0,
			want:    []string{"interpolateParams=true"},
			notWant: []string{"max_execution_time"},
		},
		{
			name:    "postgres url gets statement_timeout query parameter",
			driver:  "postgres",
			dsn:     "postgres://kora:pw@127.0.0.1:5432/app?sslmode=disable",
			timeout: 30 * time.Second,
			want:    []string{"sslmode=disable&statement_timeout=30000"},
		},
		{
			name:    "postgres url without query starts one",
			driver:  "postgres",
			dsn:     "postgres://kora:pw@127.0.0.1:5432/app",
			timeout: 500 * time.Millisecond,
			want:    []string{"/app?statement_timeout=500"},
		},
		{
			name:    "postgres keyword dsn appends statement_timeout",
			driver:  "postgres",
			dsn:     "host=127.0.0.1 dbname=app",
			timeout: 30 * time.Second,
			want:    []string{"host=127.0.0.1 dbname=app statement_timeout=30000"},
		},
		{
			name:    "postgres maintenance is unchanged",
			driver:  "postgres",
			dsn:     "postgres://kora:pw@127.0.0.1:5432/app",
			timeout: 0,
			want:    []string{"postgres://kora:pw@127.0.0.1:5432/app"},
			notWant: []string{"statement_timeout"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TuneDSN(tt.driver, tt.dsn, tt.timeout)
			if err != nil {
				t.Fatalf("TuneDSN() error = %v", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("TuneDSN() = %q, want it to contain %q", got, w)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("TuneDSN() = %q, must not contain %q", got, nw)
				}
			}
		})
	}
}

func TestStatementTimeoutFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset uses default", raw: "", want: defaultStatementTimeout},
		{name: "milliseconds", raw: "500", want: 500 * time.Millisecond},
		{name: "zero disables", raw: "0", want: 0},
		{name: "negative rejected", raw: "-1", wantErr: true},
		{name: "text rejected", raw: "soon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KORA_DB_STATEMENT_TIMEOUT_MS", tt.raw)
			got, err := statementTimeout()
			if (err != nil) != tt.wantErr {
				t.Fatalf("statementTimeout() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("statementTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}
