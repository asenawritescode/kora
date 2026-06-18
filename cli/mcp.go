package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/mcp"
	"github.com/asenawritescode/kora/site"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start MCP server (stdio mode for Claude Desktop, Cursor, etc.)",
	Long: `Starts a Model Context Protocol server on stdio that auto-generates
tools from the site's doctype registry.

Configure Claude Desktop to use it:
  {
    "mcpServers": {
      "airtime": {
        "command": "/path/to/kora",
        "args": ["mcp", "--site", "airtime.local"]
      }
    }
  }`,
	RunE: func(cmd *cobra.Command, args []string) error {
		siteName, _ := cmd.Flags().GetString("site")
		if siteName == "" {
			return fmt.Errorf("--site is required")
		}

		common := site.CommonConfigFromEnv()
		cfg := site.ReconstructSiteConfig(siteName, common, nil)

		db, err := site.Connect(cfg)
		if err != nil {
			return fmt.Errorf("connecting to database: %w", err)
		}
		defer db.Close()

		// Load config from DB.
		store := configstore.NewStore(db, kdb.Resolve(cfg.DBType))
		doctypes, err := store.LoadAll()
		if err != nil {
			return fmt.Errorf("loading doctypes: %w", err)
		}
		roles, _ := store.LoadRoles()
		permissions, _ := store.LoadPermissions()

		reg := doctype.NewRegistry()
		reg.LoadFull(doctypes, roles, permissions)

		server := mcp.New(reg, siteName)
		return server.Run(context.Background())
	},
}

func init() {
	mcpCmd.Flags().String("site", "", "Site hostname (e.g., airtime.local)")
	rootCmd.AddCommand(mcpCmd)
}
