package cli

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
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
	Long:  `Backfill historical data and check analytics pipeline status.`,
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
	common := site.CommonConfigFromEnv()
	dialect := kdb.Resolve(common.DBType)
	startup := site.LoadStartupConfig()

	platformDB, err := sql.Open(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("connecting to platform DB: %w", err)
	}
	defer platformDB.Close()

	sites, err := site.DiscoverSitesFromDB(platformDB)
	if err != nil {
		return fmt.Errorf("discovering sites: %w", err)
	}

	var target *site.DBSiteInfo
	for _, s := range sites {
		if s.Name == backfillSite {
			target = &s
			break
		}
	}
	if target == nil {
		return fmt.Errorf("site %q not found", backfillSite)
	}

	siteCfg := site.ReconstructSiteConfig(target.Name, common, target.Domains)
	db, err := site.Connect(siteCfg)
	if err != nil {
		return fmt.Errorf("connecting to site DB: %w", err)
	}
	defer db.Close()

	if err := analytics.BootstrapTables(db, dialect); err != nil {
		return fmt.Errorf("bootstrapping analytics tables: %w", err)
	}

	store := configstore.NewStore(db, dialect)
	allDTs, err := store.LoadAll()
	if err != nil {
		return fmt.Errorf("loading doctypes: %w", err)
	}

	fromDate := time.Now().AddDate(0, 0, -30)
	if backfillFrom != "" {
		if t, err := time.Parse("2006-01-02", backfillFrom); err == nil {
			fromDate = t
		}
	}

	for _, dt := range allDTs {
		if backfillDoctype != "" && dt.Name != backfillDoctype {
			continue
		}

		metrics := analytics.GenerateMetrics(dt)
		if dt.IsSubmittable {
			workflows, _ := store.LoadWorkflows()
			for _, wf := range workflows {
				if wf.DocumentType == dt.Name {
					metrics = append(metrics, analytics.GenerateWorkflowMetrics(dt, wf)...)
					break
				}
			}
		}

		fmt.Printf("Backfilling %s (%d metrics)...\n", dt.Name, len(metrics))
		for _, m := range metrics {
			if err := backfillMetric(db, dialect, dt, m, fromDate); err != nil {
				slog.Warn("backfill metric failed", "metric", m.Name, "error", err)
			}
		}
		fmt.Printf("  Done: %s\n", dt.Name)
	}

	fmt.Println("Backfill complete.")
	return nil
}

func runAnalyticsStatus(cmd *cobra.Command, args []string) error {
	common := site.CommonConfigFromEnv()
	siteCfg := site.ReconstructSiteConfig(backfillSite, common, nil)
	db, err := site.Connect(siteCfg)
	if err != nil {
		return fmt.Errorf("connecting to site DB: %w", err)
	}
	defer db.Close()

	var daily, monthly, workflow int
	db.QueryRow("SELECT COUNT(*) FROM _kora_analytics_daily WHERE site = ?", backfillSite).Scan(&daily)
	db.QueryRow("SELECT COUNT(*) FROM _kora_analytics_monthly WHERE site = ?", backfillSite).Scan(&monthly)
	db.QueryRow("SELECT COUNT(*) FROM _kora_analytics_workflow WHERE site = ?", backfillSite).Scan(&workflow)

	fmt.Printf("Site: %s\n", backfillSite)
	fmt.Printf("Daily rollup rows:    %d\n", daily)
	fmt.Printf("Monthly rollup rows:  %d\n", monthly)
	fmt.Printf("Workflow event rows:  %d\n", workflow)
	return nil
}

func backfillMetric(db *sql.DB, dialect kdb.Dialect, dt *doctype.DocType, m *analytics.Metric, from time.Time) error {
	q := dialect.QuoteIdent
	table := dt.TableName()
	upsert := dialect.UpsertIncrement(
		[]string{"site", "doctype", "metric", "dimension", "date"},
		[]string{"value"},
	)

	switch m.Type {
	case analytics.MetricCount, analytics.MetricCountByTime:
		query := fmt.Sprintf(
			`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value)
			 SELECT ?, ?, ?, '', DATE(creation), COUNT(*)
			 FROM %s WHERE creation >= ?
			 GROUP BY DATE(creation)
			 %s`,
			table, upsert,
		)
		_, err := db.Exec(query, backfillSite, dt.Name, m.Name, from)
		return err

	case analytics.MetricCountByField:
		col := q(m.Field)
		query := fmt.Sprintf(
			`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value)
			 SELECT ?, ?, ?, CONCAT('%s=', %s), DATE(creation), COUNT(*)
			 FROM %s WHERE creation >= ? AND %s IS NOT NULL AND %s != ''
			 GROUP BY %s, DATE(creation)
			 %s`,
			m.Field, col, table, col, col, col, upsert,
		)
		_, err := db.Exec(query, backfillSite, dt.Name, m.Name, m.Field, from)
		return err

	case analytics.MetricSum:
		col := q(m.Field)
		query := fmt.Sprintf(
			`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value)
			 SELECT ?, ?, ?, '', DATE(creation), SUM(%s)
			 FROM %s WHERE creation >= ? AND %s IS NOT NULL
			 GROUP BY DATE(creation)
			 %s`,
			col, table, col, upsert,
		)
		_, err := db.Exec(query, backfillSite, dt.Name, m.Name, from)
		return err

	default:
		return nil
	}
}
