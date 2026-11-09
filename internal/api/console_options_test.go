package api

import (
	"database/sql"
	"testing"
)

// consoleSettings is the part of a ConsoleHandler that ConsoleOptions sets.
type consoleSettings struct {
	dbType, dbHost string
	dbPort         int
	dbUser, dbPass string
	platformDB     *sql.DB
	onboarding     bool
}

func TestNewConsoleHandlerOptions(t *testing.T) {
	database := &sql.DB{}
	tests := []struct {
		name string
		opts ConsoleOptions
		want consoleSettings
	}{
		{"zero value has no platform database and onboarding off", ConsoleOptions{}, consoleSettings{}},
		{
			"every option reaches the handler",
			ConsoleOptions{DBType: "mysql", DBHost: "db.internal", DBPort: 3307, DBUser: "root", DBPassword: "secret", PlatformDB: database, AllowOnboarding: true},
			consoleSettings{"mysql", "db.internal", 3307, "root", "secret", database, true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewConsoleHandler(nil, nil, tt.opts)
			got := consoleSettings{handler.PlatformDBType, handler.PlatformDBHost, handler.PlatformDBPort, handler.PlatformDBUser, handler.PlatformDBPassword, handler.PlatformDB, handler.AllowOnboarding}
			if got != tt.want {
				t.Fatalf("handler settings = %+v, want %+v", got, tt.want)
			}
			if handler.ProvisioningStore == nil || handler.queuedJobs == nil {
				t.Fatal("handler was built without its provisioning store and job queue")
			}
		})
	}
}
