// Package main — Kigae Fieldwork Sample Application
//
// This is the entry point for the Kigae Fieldwork app. It starts the Kigae engine
// which loads site configs, bootstraps the database, builds the DocType registry,
// runs schema migrations, and starts the HTTP server.
//
// The application is defined entirely in YAML config files under config/v0/fieldwork/.
// No application-specific Go code is needed beyond what the engine provides.
// Custom business logic can be added via Go hooks (see hooks/ directory).
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asenawritescode/kora/cli"
	_ "github.com/go-sql-driver/mysql"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var outcome *cli.CLIExitError
		if errors.As(err, &outcome) {
			os.Exit(outcome.Code)
		}
		os.Exit(1)
	}
}
