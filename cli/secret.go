package cli

import (
	"database/sql"
	"fmt"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/secret"
	"github.com/asenawritescode/kora/site"
	"github.com/spf13/cobra"
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage encrypted secrets (API keys, credentials)",
}

var secretSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set an encrypted secret",
	RunE: func(cmd *cobra.Command, args []string) error {
		siteName, _ := cmd.Flags().GetString("site")
		key, _ := cmd.Flags().GetString("key")
		value, _ := cmd.Flags().GetString("value")
		if siteName == "" || key == "" || value == "" {
			return fmt.Errorf("--site, --key, and --value are required")
		}

		db, dialect, err := loadSiteDB(siteName)
		if err != nil {
			return err
		}
		defer db.Close()
		store := secret.NewStore(db, dialect)
		if err := store.Set(siteName, key, value); err != nil {
			return fmt.Errorf("setting secret: %w", err)
		}
		fmt.Printf("✓ Secret %q set for site %q\n", key, siteName)
		return nil
	},
}

func loadSiteDB(siteName string) (*sql.DB, kdb.Dialect, error) {
	common := site.CommonConfigFromEnv()
	cfg := site.ReconstructSiteConfig(siteName, common, nil)
	db, err := site.Connect(cfg)
	if err != nil {
		return nil, nil, err
	}
	return db, kdb.Resolve(cfg.DBType), nil
}

var secretGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Get a decrypted secret",
	RunE: func(cmd *cobra.Command, args []string) error {
		siteName, _ := cmd.Flags().GetString("site")
		key, _ := cmd.Flags().GetString("key")
		if siteName == "" || key == "" {
			return fmt.Errorf("--site and --key are required")
		}
		db, dialect, err := loadSiteDB(siteName)
		if err != nil {
			return err
		}
		defer db.Close()
		store := secret.NewStore(db, dialect)
		val, err := store.Get(siteName, key)
		if err != nil {
			return fmt.Errorf("getting secret: %w", err)
		}
		fmt.Print(val)
		return nil
	},
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secret key names (not values)",
	RunE: func(cmd *cobra.Command, args []string) error {
		siteName, _ := cmd.Flags().GetString("site")
		if siteName == "" {
			return fmt.Errorf("--site is required")
		}
		db, dialect, err := loadSiteDB(siteName)
		if err != nil {
			return err
		}
		defer db.Close()
		store := secret.NewStore(db, dialect)
		keys, err := store.List(siteName)
		if err != nil {
			return fmt.Errorf("listing secrets: %w", err)
		}
		for _, k := range keys {
			fmt.Println(k)
		}
		return nil
	},
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a secret",
	RunE: func(cmd *cobra.Command, args []string) error {
		siteName, _ := cmd.Flags().GetString("site")
		key, _ := cmd.Flags().GetString("key")
		if siteName == "" || key == "" {
			return fmt.Errorf("--site and --key are required")
		}
		db, dialect, err := loadSiteDB(siteName)
		if err != nil {
			return err
		}
		defer db.Close()
		store := secret.NewStore(db, dialect)
		if err := store.Delete(siteName, key); err != nil {
			return fmt.Errorf("deleting secret: %w", err)
		}
		fmt.Printf("✓ Secret %q deleted from site %q\n", key, siteName)
		return nil
	},
}

func init() {
	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretGetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretCmd.AddCommand(secretDeleteCmd)

	for _, c := range []*cobra.Command{secretSetCmd, secretGetCmd, secretListCmd, secretDeleteCmd} {
		c.Flags().String("site", "", "Site hostname (e.g., airtime.local)")
	}
	for _, c := range []*cobra.Command{secretSetCmd, secretGetCmd, secretDeleteCmd} {
		c.Flags().String("key", "", "Secret key name")
	}
	secretSetCmd.Flags().String("value", "", "Secret value to encrypt and store")
}
