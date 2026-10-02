package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/spf13/cobra"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/site"
)

var siteDirectoryCmd = &cobra.Command{
	Use:   "site-directory",
	Short: "Manage the Engine site directory",
}

var siteDirectoryMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply additive site identity and alias-index migrations",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		startup := site.LoadStartupConfig()
		if err := startup.Validate(); err != nil {
			return err
		}
		if startup.DBDSN == "" {
			return fmt.Errorf("DB_DSN is required to migrate the site directory")
		}
		database, err := sql.Open(startup.DBType, startup.DBDSN)
		if err != nil {
			return fmt.Errorf("open platform registry database: %w", err)
		}
		defer database.Close()
		if err := database.Ping(); err != nil {
			return fmt.Errorf("ping platform registry database: %w", err)
		}
		if err := site.BootstrapPlatformRegistry(database, kdb.Resolve(startup.DBType)); err != nil {
			return fmt.Errorf("migrate site directory: %w", err)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Site directory identity and alias projection are migrated.")
		return err
	},
}

type siteProbeResult struct {
	SiteID          string  `json:"site_id,omitempty"`
	Hostname        string  `json:"hostname"`
	LatencyMS       float64 `json:"latency_ms"`
	OpenConnections int     `json:"open_connections_after_ping"`
	IdleConnections int     `json:"idle_connections_after_ping"`
	Failed          bool    `json:"failed"`
}

var siteDirectoryProbeCmd = &cobra.Command{
	Use:   "probe",
	Short: "Measure read-only site database connection latency",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		report, err := runSiteDirectoryProbe()
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	},
}

var siteDirectoryPruneOlderThan time.Duration
var siteDirectoryPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Compact site directory history already applied by every consumer",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if siteDirectoryPruneOlderThan <= 0 {
			return fmt.Errorf("--older-than must be a positive duration")
		}
		startup := site.LoadStartupConfig()
		if err := startup.Validate(); err != nil {
			return err
		}
		if startup.DBDSN == "" {
			return fmt.Errorf("DB_DSN is required to prune site directory history")
		}
		database, err := sql.Open(startup.DBType, startup.DBDSN)
		if err != nil {
			return fmt.Errorf("open platform registry database: %w", err)
		}
		defer database.Close()
		if err := database.Ping(); err != nil {
			return fmt.Errorf("ping platform registry database: %w", err)
		}
		registry := site.NewSQLSiteRegistry(database, startup.DBType)
		deleted, err := registry.PruneOutboxBefore(time.Now().Add(-siteDirectoryPruneOlderThan))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Pruned %d applied site-directory changes.\n", deleted)
		return err
	},
}

type siteDirectoryProbeReport struct {
	ReadOnly     bool              `json:"read_only"`
	SiteCount    int               `json:"site_count"`
	Concurrency  int               `json:"concurrency"`
	TotalWallMS  float64           `json:"total_wall_ms"`
	P50LatencyMS float64           `json:"p50_latency_ms"`
	P95LatencyMS float64           `json:"p95_latency_ms"`
	Sites        []siteProbeResult `json:"sites"`
}

func runSiteDirectoryProbe() (siteDirectoryProbeReport, error) {
	startup := site.LoadStartupConfig()
	if err := startup.Validate(); err != nil {
		return siteDirectoryProbeReport{}, err
	}
	if startup.DBDSN == "" {
		return siteDirectoryProbeReport{}, fmt.Errorf("DB_DSN is required for the read-only site probe")
	}
	platformDB, err := sql.Open(startup.DBType, startup.DBDSN)
	if err != nil {
		return siteDirectoryProbeReport{}, fmt.Errorf("open platform registry database: %w", err)
	}
	defer platformDB.Close()
	if err := platformDB.Ping(); err != nil {
		return siteDirectoryProbeReport{}, fmt.Errorf("ping platform registry database: %w", err)
	}
	dbSites, err := site.DiscoverSitesFromDB(platformDB)
	if err != nil {
		return siteDirectoryProbeReport{}, fmt.Errorf("read site directory snapshot: %w", err)
	}
	if len(dbSites) == 0 {
		return siteDirectoryProbeReport{ReadOnly: true}, nil
	}
	concurrency := siteStartupParallelism
	if concurrency > len(dbSites) {
		concurrency = len(dbSites)
	}
	report := siteDirectoryProbeReport{ReadOnly: true, SiteCount: len(dbSites), Concurrency: concurrency, Sites: make([]siteProbeResult, len(dbSites))}
	started := time.Now()
	common := site.CommonConfigFromEnv()
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				info := dbSites[index]
				result := siteProbeResult{SiteID: info.SiteID, Hostname: info.Name}
				begin := time.Now()
				database, connectErr := site.Connect(site.ReconstructSiteConfigFromDBInfo(info, common))
				result.LatencyMS = float64(time.Since(begin).Microseconds()) / 1000
				if connectErr != nil {
					result.Failed = true
				} else {
					stats := database.Stats()
					result.OpenConnections = stats.OpenConnections
					result.IdleConnections = stats.Idle
					_ = database.Close()
				}
				report.Sites[index] = result
			}
		}()
	}
	for index := range dbSites {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	report.TotalWallMS = float64(time.Since(started).Microseconds()) / 1000
	latencies := make([]float64, 0, len(report.Sites))
	for _, item := range report.Sites {
		if !item.Failed {
			latencies = append(latencies, item.LatencyMS)
		}
	}
	sort.Float64s(latencies)
	if len(latencies) > 0 {
		report.P50LatencyMS = percentile(latencies, .50)
		report.P95LatencyMS = percentile(latencies, .95)
	}
	return report, nil
}

func percentile(sorted []float64, percentile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(float64(len(sorted))*percentile)) - 1
	return sorted[index]
}

func init() {
	siteDirectoryCmd.AddCommand(siteDirectoryMigrateCmd)
	siteDirectoryCmd.AddCommand(siteDirectoryListCmd)
	siteDirectoryCmd.AddCommand(siteDirectoryAssignCellCmd)
	siteDirectoryCmd.AddCommand(siteDirectoryProbeCmd)
	siteDirectoryPruneCmd.Flags().DurationVar(&siteDirectoryPruneOlderThan, "older-than", 0, "retain at least this much directory history (for example 720h)")
	siteDirectoryCmd.AddCommand(siteDirectoryPruneCmd)
}
