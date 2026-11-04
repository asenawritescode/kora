package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/site"
)

var (
	backfillSite    string
	backfillDoctype string
	backfillFrom    string
)

func init() {
	analyticsBackfillCmd.Flags().StringVar(&backfillSite, "site", "", "Site hostname (required)")
	analyticsBackfillCmd.Flags().StringVar(&backfillDoctype, "doctype", "", "DocType to backfill (default: all)")
	analyticsBackfillCmd.Flags().StringVar(&backfillFrom, "from", "", "Start date YYYY-MM-DD (default: 30 days ago)")
	analyticsBackfillCmd.MarkFlagRequired("site")

	analyticsStatusCmd.Flags().StringVar(&backfillSite, "site", "", "Site hostname (required)")
	analyticsStatusCmd.MarkFlagRequired("site")

	analyticsCmd.AddCommand(analyticsBackfillCmd)
	analyticsCmd.AddCommand(analyticsStatusCmd)
	rootCmd.AddCommand(analyticsCmd)
}

var analyticsCmd = &cobra.Command{
	Use:   "analytics",
	Short: "Analytics management",
}

var analyticsBackfillCmd = &cobra.Command{
	Use:   "backfill --site <site> [--doctype <doctype>] [--from <date>]",
	Short: "Backfill historical data into analytics rollup tables",
	RunE:  runAnalyticsBackfill,
}

var analyticsStatusCmd = &cobra.Command{
	Use:   "status --site <site>",
	Short: "Show analytics rollup status for a site",
	RunE:  runAnalyticsStatus,
}

func runAnalyticsBackfill(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	common := site.CommonConfigFromEnv()
	startup := site.LoadStartupConfig()

	platformDB, err := dbaccess.Open(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("connecting to platform DB: %w", err)
	}
	defer platformDB.Close()
	platformAdmission, err := dbaccess.AdmissionFor(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("resolve platform work admission: %w", err)
	}
	if err := runCLIWork(ctx, platformAdmission, func() error { return platformDB.PingContext(ctx) }); err != nil {
		return fmt.Errorf("ping platform DB: %w", err)
	}
	var target site.DBSiteInfo
	err = runCLIWork(ctx, platformAdmission, func() error {
		id, resolveErr := site.ResolveSiteAliasContext(ctx, platformDB, startup.DBType, backfillSite)
		if resolveErr != nil {
			return resolveErr
		}
		target, resolveErr = site.NewSQLSiteRegistry(platformDB, startup.DBType).GetByIDContext(ctx, id)
		return resolveErr
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("site %q not found", backfillSite)
		}
		return fmt.Errorf("resolving site %q: %w", backfillSite, err)
	}

	siteCfg := site.ReconstructSiteConfigFromDBInfo(target, common)
	dialect := kdb.Resolve(siteCfg.DBType)
	release, err := acquireSiteCLIWork(ctx, siteCfg, true)
	if err != nil {
		return err
	}
	database, err := site.ConnectContext(ctx, siteCfg)
	if err != nil {
		release()
		return fmt.Errorf("connecting to site DB: %w", err)
	}
	defer database.Close()
	defer release()

	store := configstore.NewStore(database, dialect)
	docTypes, err := store.LoadAll(target.Name)
	if err != nil {
		return fmt.Errorf("loading doctypes: %w", err)
	}
	registry := doctype.NewRegistry()
	for _, dt := range docTypes {
		registry.Register(dt)
	}
	if backfillDoctype != "" && registry.Get(backfillDoctype) == nil {
		return fmt.Errorf("doctype %q not found on site %q", backfillDoctype, target.Name)
	}

	from := time.Now().UTC().AddDate(0, 0, -30)
	if backfillFrom != "" {
		from, err = time.Parse("2006-01-02", backfillFrom)
		if err != nil {
			return fmt.Errorf("invalid --from date %q: expected YYYY-MM-DD", backfillFrom)
		}
	}
	metrics, err := analytics.BackfillContext(ctx, database, dialect, target.Name, registry, from, backfillDoctype)
	if err != nil {
		return fmt.Errorf("analytics rebuild failed: %w", err)
	}
	fmt.Printf("Backfill complete: %d metrics rebuilt.\n", metrics)
	return nil
}

func runAnalyticsStatus(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	common := site.CommonConfigFromEnv()
	siteCfg := site.ReconstructSiteConfig(backfillSite, common, nil)
	release, err := acquireSiteCLIWork(ctx, siteCfg, false)
	if err != nil {
		return err
	}
	database, err := site.ConnectContext(ctx, siteCfg)
	if err != nil {
		release()
		return fmt.Errorf("connecting to site DB: %w", err)
	}
	defer database.Close()
	defer release()

	dialect := kdb.Resolve(siteCfg.DBType)
	var daily, monthly, workflow int
	for _, count := range []struct {
		table string
		dest  *int
	}{{"_kora_analytics_daily", &daily}, {"_kora_analytics_monthly", &monthly}, {"_kora_analytics_workflow", &workflow}} {
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE site = ?", dialect.QuoteIdent(count.table))
		if err := database.QueryRowContext(ctx, kdb.Rebind(dialect, query), backfillSite).Scan(count.dest); err != nil {
			return fmt.Errorf("reading %s analytics status: %w", count.table, err)
		}
	}

	fmt.Printf("Site: %s\n", backfillSite)
	fmt.Printf("Daily rollup rows:    %d\n", daily)
	fmt.Printf("Monthly rollup rows:  %d\n", monthly)
	fmt.Printf("Workflow event rows:  %d\n", workflow)
	return nil
}
