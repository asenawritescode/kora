package cli

import (
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/site"
)

var (
	migrateSiteFlag   string
	migrateAllFlag    bool
	allowBreakingFlag bool
)

func init() {
	migrateCmd.Flags().StringVar(&migrateSiteFlag, "site", "", "Target site hostname")
	migrateCmd.Flags().BoolVar(&migrateAllFlag, "all", false, "Migrate all sites")
	migrateCmd.Flags().BoolVar(&allowBreakingFlag, "allow-breaking", false, "Allow breaking schema changes")
}

func runMigrate() error {
	if migrateSiteFlag == "" && !migrateAllFlag {
		return fmt.Errorf("specify --site <hostname> or --all")
	}

	var hostnames []string
	if migrateAllFlag {
		discovered, err := site.DiscoverSites("sites")
		if err != nil {
			return fmt.Errorf("discovering sites: %w", err)
		}
		hostnames = discovered
	} else {
		hostnames = []string{migrateSiteFlag}
	}

	for _, hostname := range hostnames {
		slog.Info("migrating site", "site", hostname)

		siteCfg, err := site.LoadSiteConfig(fmt.Sprintf("sites/%s/site_config.yaml", hostname))
		if err != nil {
			return fmt.Errorf("loading site config for %s: %w", hostname, err)
		}

		db, err := site.Connect(siteCfg)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", hostname, err)
		}

		// Bootstrap system tables.
		if err := site.BootstrapSystemTables(db); err != nil {
			db.Close()
			return fmt.Errorf("bootstrapping %s: %w", hostname, err)
		}

		// Load config from DB.
		store := configstore.NewStore(db)
		doctypes, err := store.LoadAll()
		if err != nil {
			db.Close()
			return fmt.Errorf("loading config for %s: %w", hostname, err)
		}

		if len(doctypes) == 0 {
			slog.Warn("no DocTypes found", "site", hostname)
			db.Close()
			continue
		}

		registry := doctype.NewRegistry()
		registry.LoadFromDB(doctypes)

		if err := schema.MigrateSite(db, siteCfg.DBName, registry); err != nil {
			db.Close()
			return fmt.Errorf("migrating %s: %w", hostname, err)
		}

		db.Close()
		fmt.Printf("  ✓ %s migrated\n", hostname)
	}

	fmt.Println("Migration complete.")
	return nil
}
