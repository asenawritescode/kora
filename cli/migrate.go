package cli

import (
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
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
	migrateCmd.Flags().StringVar(&migrateSiteFlag, "site", "", "Target site info.Name")
	migrateCmd.Flags().BoolVar(&migrateAllFlag, "all", false, "Migrate all sites")
	migrateCmd.Flags().BoolVar(&allowBreakingFlag, "allow-breaking", false, "Allow breaking schema changes")
}

func runMigrate() error {
	if migrateSiteFlag == "" && !migrateAllFlag {
		return fmt.Errorf("specify --site <info.Name> or --all")
	}

	common := site.CommonConfigFromEnv()

	var dbSites []site.DBSiteInfo
	if migrateAllFlag {
		// Discover sites from database.
		cfg := site.ReconstructSiteConfig("_discovery_", common, nil)
		db, err := site.Connect(cfg)
		if err != nil {
			return fmt.Errorf("connecting to platform db: %w", err)
		}
		dbSites, err = site.DiscoverSitesFromDB(db)
		db.Close()
		if err != nil {
			return fmt.Errorf("discovering sites: %w", err)
		}
	} else {
		dbSites = []site.DBSiteInfo{{Name: migrateSiteFlag}}
	}

	for _, info := range dbSites {
		slog.Info("migrating site", "site", info.Name)

		siteCfg := site.ReconstructSiteConfig(info.Name, common, info.Domains)

		db, err := site.Connect(siteCfg)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", info.Name, err)
		}

		// Bootstrap system tables.
		if err := site.BootstrapSystemTables(db, kdb.Resolve(siteCfg.DBType)); err != nil {
			db.Close()
			return fmt.Errorf("bootstrapping %s: %w", info.Name, err)
		}

		// Load config from DB.
		store := configstore.NewStore(db, kdb.Resolve(siteCfg.DBType))
		doctypes, err := store.LoadAll(info.Name)
		if err != nil {
			db.Close()
			return fmt.Errorf("loading config for %s: %w", info.Name, err)
		}

		if len(doctypes) == 0 {
			slog.Warn("no DocTypes found", "site", info.Name)
			db.Close()
			continue
		}

		registry := doctype.NewRegistry()
		registry.LoadFromDB(doctypes)

		if err := schema.MigrateSite(db, siteCfg.DBName, registry, kdb.Resolve(siteCfg.DBType)); err != nil {
			db.Close()
			return fmt.Errorf("migrating %s: %w", info.Name, err)
		}

		db.Close()
		fmt.Printf("  ✓ %s migrated\n", info.Name)
	}

	fmt.Println("Migration complete.")
	return nil
}
