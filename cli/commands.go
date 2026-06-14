package cli

import (
	"github.com/spf13/cobra"
)

// Execute runs the root command. This is the main entry point for the CLI.
func Execute() error {
	return rootCmd.Execute()
}

var rootCmd = &cobra.Command{
	Use:   "kora",
	Short: "Kora — Config-Driven Application Engine",
	Long: `Kora is a config-driven application engine.
You define your application — its data model, rules, permissions, and workflows —
in structured configuration. The engine reads that configuration and provides a
fully functional backend: database schema, REST API, admin UI, and background jobs.`,
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(migrateCmd)
	rootCmd.AddCommand(newSiteCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(secretCmd)
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTP server",
	Long:  `Start the Kora HTTP server, serving all configured sites.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe()
	},
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply schema migrations",
	Long:  `Apply pending schema migrations for one or all sites.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMigrate()
	},
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage application configuration",
	Long:  `Import, export, diff, and apply config versions.`,
}
