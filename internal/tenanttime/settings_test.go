package tenanttime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/internal/db"
)

func TestParseZone(t *testing.T) {
	for _, tt := range []struct {
		name string
		ok   bool
	}{
		{"Africa/Nairobi", true},
		{" Europe/London ", true},
		{"UTC", true},
		{"Local", false},
		{"", false},
		{"Mars/Olympus", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseZone(tt.name)
			if (err == nil) != tt.ok {
				t.Fatalf("ParseZone(%q) error = %v, want ok=%v", tt.name, err, tt.ok)
			}
		})
	}
}

func TestParseCutoff(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  time.Duration
		ok    bool
	}{
		{"00:00", 0, true},
		{"03:00", 3 * time.Hour, true},
		{"06:00", 6 * time.Hour, true},
		{"06:30", 0, false},
		{"07:00", 0, false},
		{"3:00", 0, false},
		{"03:00:00", 0, false},
		{"", 0, false},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseCutoff(tt.input)
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("ParseCutoff(%q) = %s, %v", tt.input, got, err)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	for _, tt := range []struct {
		name   string
		zone   *string
		cutoff *string
		fail   bool
		want   Clock
	}{
		{name: "unset keys use defaults", want: Default()},
		{name: "stored values apply", zone: strPtr("Europe/London"), cutoff: strPtr("03:00"),
			want: Clock{Location: mustLoad(t, "Europe/London"), Cutoff: 3 * time.Hour}},
		{name: "stored zone that no longer parses fails", zone: strPtr("Mars/Olympus"), fail: true},
		{name: "stored cutoff that no longer parses fails", cutoff: strPtr("09:00"), fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			expectSetting(mock, "time_zone", tt.zone)
			expectSetting(mock, "business_day_cutoff", tt.cutoff)

			got, err := Load(context.Background(), conn, db.Resolve("mysql"), "s1")
			if tt.fail {
				if err == nil {
					t.Fatalf("Load accepted invalid stored value: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Location.String() != tt.want.Location.String() || got.Cutoff != tt.want.Cutoff {
				t.Fatalf("Load = %s %s, want %s %s", got.Location, got.Cutoff, tt.want.Location, tt.want.Cutoff)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadReturnsReadError(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting").WillReturnError(errors.New("connection refused"))
	if _, err := Load(context.Background(), conn, db.Resolve("mysql"), "s1"); err == nil {
		t.Fatal("Load hid a database error")
	}
}

func expectSetting(mock sqlmock.Sqlmock, key string, value *string) {
	query := mock.ExpectQuery("SELECT setting_value FROM _kora_site_setting WHERE site = \\? AND setting_key = \\?").WithArgs("s1", key)
	if value == nil {
		query.WillReturnRows(sqlmock.NewRows([]string{"setting_value"}))
		return
	}
	query.WillReturnRows(sqlmock.NewRows([]string{"setting_value"}).AddRow(*value))
}

func strPtr(s string) *string { return &s }
