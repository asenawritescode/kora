package scheduler

import (
	"testing"
	"time"
)

func TestParseCron_EveryMinute(t *testing.T) {
	c := parseCron("* * * * *")
	if c == nil {
		t.Fatal("parseCron returned nil")
	}
	if c.minute != "*" {
		t.Errorf("minute = %q, want %q", c.minute, "*")
	}
	if c.hour != "*" {
		t.Errorf("hour = %q, want %q", c.hour, "*")
	}
	if c.dom != "*" {
		t.Errorf("dom = %q, want %q", c.dom, "*")
	}
	if c.month != "*" {
		t.Errorf("month = %q, want %q", c.month, "*")
	}
	if c.dow != "*" {
		t.Errorf("dow = %q, want %q", c.dow, "*")
	}
}

func TestParseCron_SpecificHour(t *testing.T) {
	tests := []struct {
		name        string
		expr        string
		wantMinute  string
		wantHour    string
		wantInvalid bool
	}{
		{
			name:       "every hour at minute 30",
			expr:       "30 * * * *",
			wantMinute: "30",
			wantHour:   "*",
		},
		{
			name:       "daily at 9am",
			expr:       "0 9 * * *",
			wantMinute: "0",
			wantHour:   "9",
		},
		{
			name:       "specific time with dom",
			expr:       "15 14 1 * *",
			wantMinute: "15",
			wantHour:   "14",
			wantInvalid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := parseCron(tt.expr)
			if c.minute != tt.wantMinute {
				t.Errorf("minute = %q, want %q", c.minute, tt.wantMinute)
			}
			if c.hour != tt.wantHour {
				t.Errorf("hour = %q, want %q", c.hour, tt.wantHour)
			}
		})
	}
}

func TestParseCron_Invalid(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{"empty string", ""},
		{"too few fields", "30 * *"},
		{"only one field", "30"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := parseCron(tt.expr)
			// Invalid cron should return default (every hour at minute 0).
			if c == nil {
				t.Fatal("parseCron should not return nil for invalid expressions")
			}
			if c.minute != "0" {
				t.Errorf("invalid cron: minute = %q, want default %q", c.minute, "0")
			}
		})
	}
}

func TestNextRun_EveryMinute(t *testing.T) {
	c := parseCron("* * * * *")
	next := c.nextRun()
	now := time.Now()

	if next.Before(now) {
		t.Errorf("nextRun before now: %v < %v", next, now)
	}

	// Should be within the next 2 minutes.
	if next.After(now.Add(2 * time.Minute)) {
		t.Errorf("nextRun too far: %v", next)
	}

	if next.Second() != 0 {
		t.Errorf("nextRun should be on a minute boundary, got second = %d", next.Second())
	}
}

func TestNextRun_SpecificTime(t *testing.T) {
	// Test with a specific hour:minute.
	now := time.Now()

	// Pick a time in the past today.
	pastHour := (now.Hour() - 1 + 24) % 24
	expr := "30 " + itoa(pastHour) + " * * *"
	c := parseCron(expr)
	next := c.nextRun()

	todayExpected := time.Date(now.Year(), now.Month(), now.Day(), pastHour, 30, 0, 0, now.Location())

	if todayExpected.Before(now) {
		// Should have wrapped to tomorrow.
		tomorrowExpected := todayExpected.Add(24 * time.Hour)
		if !next.Equal(tomorrowExpected) {
			t.Errorf("nextRun = %v, want %v (wrapped to tomorrow)", next, tomorrowExpected)
		}
	} else {
		if !next.Equal(todayExpected) {
			t.Errorf("nextRun = %v, want %v", next, todayExpected)
		}
	}
}

func TestRegisterJob(t *testing.T) {
	s := &Scheduler{}
	if len(s.jobs) != 0 {
		t.Error("new scheduler should have no jobs")
	}

	job := &JobConfig{
		Name:     "test-job",
		Type:     JobDoctypeAlert,
		Schedule: "0 * * * *",
		Config:   map[string]any{"doctype": "Task"},
	}
	s.RegisterJob(job)

	if len(s.jobs) != 1 {
		t.Fatalf("jobs length = %d, want 1", len(s.jobs))
	}
	if s.jobs[0].Name != "test-job" {
		t.Errorf("job name = %q, want %q", s.jobs[0].Name, "test-job")
	}
}

func TestJobType_Constants(t *testing.T) {
	tests := []struct {
		jobType JobType
		want    string
	}{
		{JobDoctypeAlert, "doctype_alert"},
		{JobEmailReport, "email_report"},
		{JobWebhook, "webhook"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.jobType) != tt.want {
				t.Errorf("JobType = %q, want %q", string(tt.jobType), tt.want)
			}
		})
	}
}



// itoa is a simple int-to-string helper for test purposes.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}


