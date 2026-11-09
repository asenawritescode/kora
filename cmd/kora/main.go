// Command kora is the Kigae Engine: the HTTP API server, the site/config CLI, the MCP server and the
// scheduler in one binary. All behaviour lives in internal/ packages; this file only runs the CLI and
// maps its error to an exit code.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/asenawritescode/kora/internal/cli"
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
