package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
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

func runMigrate(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if migrateSiteFlag == "" && !migrateAllFlag {
		return fmt.Errorf("specify --site <info.Name> or --all")
	}

	common := site.CommonConfigFromEnv()

	var dbSites []site.DBSiteInfo
	if migrateAllFlag {
		// Discover sites from database.
		cfg := site.ReconstructSiteConfig("_discovery_", common, nil)
		release, err := acquireSiteCLIWork(ctx, cfg, false)
		if err != nil {
			return err
		}
		db, err := site.ConnectContext(ctx, cfg)
		if err != nil {
			release()
			return fmt.Errorf("connecting to platform db: %w", err)
		}
		dbSites, err = site.DiscoverSitesFromDBContext(ctx, db)
		closeErr := db.Close()
		release()
		if err != nil {
			return fmt.Errorf("discovering sites: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("closing platform db: %w", closeErr)
		}
	} else {
		dbSites = []site.DBSiteInfo{{Name: migrateSiteFlag}}
	}

	for _, info := range dbSites {
		slog.Info("migrating site", "site", info.Name)

		siteCfg := site.ReconstructSiteConfig(info.Name, common, info.Domains)
		release, err := acquireSiteCLIWork(ctx, siteCfg, true)
		if err != nil {
			return err
		}
		db, err := site.ConnectContext(ctx, siteCfg)
		if err != nil {
			release()
			return fmt.Errorf("connecting to %s: %w", info.Name, err)
		}

		// Bootstrap system tables.
		if err := site.BootstrapSystemTables(db, kdb.Resolve(siteCfg.DBType)); err != nil {
			db.Close()
			release()
			return fmt.Errorf("bootstrapping %s: %w", info.Name, err)
		}

		// Load config from DB.
		store := configstore.NewStore(db, kdb.Resolve(siteCfg.DBType))
		doctypes, err := store.LoadAll(info.Name)
		if err != nil {
			db.Close()
			release()
			return fmt.Errorf("loading config for %s: %w", info.Name, err)
		}

		if len(doctypes) == 0 {
			slog.Warn("no DocTypes found", "site", info.Name)
			db.Close()
			release()
			continue
		}

		registry := doctype.NewRegistry()
		registry.LoadFromDB(doctypes)

		if err := schema.MigrateSiteFromRegistry(db, siteCfg.DBName, registry, kdb.Resolve(siteCfg.DBType)); err != nil {
			db.Close()
			release()
			return fmt.Errorf("migrating %s: %w", info.Name, err)
		}

		if err := db.Close(); err != nil {
			release()
			return fmt.Errorf("closing %s database: %w", info.Name, err)
		}
		release()
		fmt.Printf("  ✓ %s migrated\n", info.Name)
	}

	fmt.Println("Migration complete.")
	return nil
}
