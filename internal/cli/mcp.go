package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/mcp"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the MCP YAML validation and optional read-only tenant server",
	Long: `Starts a Model Context Protocol server on stdio with YAML validation.

Tenant DocType read tools are enabled only when KORA_MCP_EXTENSION_TOKEN and
--tenant-id are configured. The extension credential is revalidated for every
call; only find, list, and get operations are exposed.

Configure Claude Desktop to use it:
  {
    "mcpServers": {
      "airtime": {
        "command": "/path/to/kora",
        "args": ["mcp", "--site", "airtime.local", "--tenant-id", "<stable-tenant-id>"]
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

		db, err := site.Connect(context.Background(), cfg)
		if err != nil {
			return fmt.Errorf("connecting to database: %w", err)
		}
		defer db.Close()

		// Load config from DB.
		store := configstore.NewStore(db, kdb.Resolve(cfg.DBType))
		doctypes, err := store.LoadAll(siteName)
		if err != nil {
			return fmt.Errorf("loading doctypes: %w", err)
		}
		roles, err := store.LoadRoles(siteName)
		if err != nil {
			return fmt.Errorf("loading roles: %w", err)
		}
		permissions, err := store.LoadPermissions(siteName)
		if err != nil {
			return fmt.Errorf("loading permissions: %w", err)
		}

		reg := doctype.NewRegistry()
		reg.LoadFull(doctypes, roles, permissions)

		server := mcp.New(reg, siteName, mcp.ModeValidationOnly)
		token := os.Getenv("KORA_MCP_EXTENSION_TOKEN")
		if token != "" {
			tenantID, _ := cmd.Flags().GetString("tenant-id")
			if tenantID == "" {
				return fmt.Errorf("--tenant-id is required when KORA_MCP_EXTENSION_TOKEN is configured")
			}
			executor, err := mcp.NewExtensionReadExecutor(db, kdb.Resolve(cfg.DBType), reg, contract.TenantID(tenantID), siteName, token)
			if err != nil {
				return fmt.Errorf("configuring authenticated read-only MCP tools: %w", err)
			}
			server = mcp.NewReadOnlyWithExecutor(reg, siteName, executor)
		}
		return server.Run(context.Background())
	},
}

func init() {
	mcpCmd.Flags().String("site", "", "Site hostname (e.g., airtime.local)")
	mcpCmd.Flags().String("tenant-id", "", "Stable tenant ID (required for authenticated MCP reads)")
	rootCmd.AddCommand(mcpCmd)
}
