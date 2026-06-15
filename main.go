// Package main — Kora Fieldwork Sample Application
//
// This is the entry point for the Kora Fieldwork app. It starts the Kora engine
// which loads site configs, bootstraps the database, builds the DocType registry,
// runs schema migrations, and starts the HTTP server.
//
// The application is defined entirely in YAML config files under config/fieldwork/.
// No application-specific Go code is needed beyond what the engine provides.
// Custom business logic can be added via Go hooks (see hooks/ directory).
package main

import (
	"fmt"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/asenawritescode/kora/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
