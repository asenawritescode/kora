package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/site"
)

var (
	factsSiteName string
	factsModel    string
	factsJobID    string
	factsWait     bool
)

// factsRetryInterval is the pause between maintenance admission attempts under --wait.
var factsRetryInterval = time.Second

func init() {
	factsRebuildCmd.Flags().StringVar(&factsSiteName, "site", "", "Site hostname (required)")
	factsRebuildCmd.Flags().StringVar(&factsModel, "model", "", "DocType whose fact model is rebuilt (required)")
	factsRebuildCmd.Flags().BoolVar(&factsWait, "wait", false, "Wait for maintenance admission instead of failing when the lane is full")
	factsRebuildCmd.MarkFlagRequired("site")
	factsRebuildCmd.MarkFlagRequired("model")

	factsRebuildStatusCmd.Flags().StringVar(&factsSiteName, "site", "", "Site hostname (required)")
	factsRebuildStatusCmd.Flags().StringVar(&factsJobID, "job", "", "Rebuild job ID (required)")
	factsRebuildStatusCmd.MarkFlagRequired("site")
	factsRebuildStatusCmd.MarkFlagRequired("job")

	factsRebuildCancelCmd.Flags().StringVar(&factsSiteName, "site", "", "Site hostname (required)")
	factsRebuildCancelCmd.Flags().StringVar(&factsJobID, "job", "", "Rebuild job ID (required)")
	factsRebuildCancelCmd.MarkFlagRequired("site")
	factsRebuildCancelCmd.MarkFlagRequired("job")

	factsCmd.AddCommand(factsRebuildCmd, factsRebuildStatusCmd, factsRebuildCancelCmd)
	rootCmd.AddCommand(factsCmd)
}

var factsCmd = &cobra.Command{
	Use:   "facts",
	Short: "Fact table management",
}

var factsRebuildCmd = &cobra.Command{
	Use:   "rebuild --site <site> --model <doctype> [--wait]",
	Short: "Rebuild a doctype's fact tables from its source tables, resuming any unfinished job",
	RunE:  runFactsRebuild,
}

var factsRebuildStatusCmd = &cobra.Command{
	Use:   "rebuild-status --site <site> --job <id>",
	Short: "Show the state, checkpoint and processed count of a rebuild job",
	RunE:  runFactsRebuildStatus,
}

var factsRebuildCancelCmd = &cobra.Command{
	Use:   "rebuild-cancel --site <site> --job <id>",
	Short: "Cancel a queued or running rebuild job",
	RunE:  runFactsRebuildCancel,
}

func runFactsRebuild(cmd *cobra.Command, _ []string) error {
	ctx := commandContext(cmd)
	siteCfg, database, release, err := openFactsSite(ctx, factsSiteName, func(ctx context.Context, cfg *site.Config) (func(), error) {
		return admitFactsMaintenance(ctx, factsWait, func(ctx context.Context) (func(), error) {
			return acquireSiteCLIWork(ctx, cfg, true)
		})
	})
	if err != nil {
		return err
	}
	defer release()
	defer database.Close()
	dialect := kdb.Resolve(siteCfg.DBType)
	registry, err := loadFactsRegistry(database, dialect, factsSiteName)
	if err != nil {
		return err
	}
	job, err := facts.RebuildModel(ctx, facts.RebuildRequest{DB: database, Dialect: dialect, Registry: registry, Site: factsSiteName, Model: factsModel})
	if err != nil {
		return err
	}
	fmt.Printf("Rebuild job %s finished: %s, %d records.\n", job.ID, job.Status, job.Processed)
	return nil
}

func runFactsRebuildStatus(cmd *cobra.Command, _ []string) error {
	ctx := commandContext(cmd)
	siteCfg, database, release, err := openFactsSite(ctx, factsSiteName, func(ctx context.Context, cfg *site.Config) (func(), error) {
		return acquireSiteCLIWork(ctx, cfg, false)
	})
	if err != nil {
		return err
	}
	defer release()
	defer database.Close()
	store := &facts.JobStore{DB: database, Dialect: kdb.Resolve(siteCfg.DBType)}
	job, err := store.Get(ctx, factsJobID)
	if err != nil {
		return fmt.Errorf("reading rebuild job %s: %w", factsJobID, err)
	}
	fmt.Print(formatFactsJob(job))
	return nil
}

func runFactsRebuildCancel(cmd *cobra.Command, _ []string) error {
	ctx := commandContext(cmd)
	siteCfg, database, release, err := openFactsSite(ctx, factsSiteName, func(ctx context.Context, cfg *site.Config) (func(), error) {
		return acquireSiteCLIWork(ctx, cfg, false)
	})
	if err != nil {
		return err
	}
	defer release()
	defer database.Close()
	store := &facts.JobStore{DB: database, Dialect: kdb.Resolve(siteCfg.DBType)}
	if err := store.Cancel(ctx, factsJobID); err != nil {
		return err
	}
	fmt.Printf("Rebuild job %s cancelled.\n", factsJobID)
	return nil
}

// admitFactsMaintenance takes a maintenance slot. A full lane fails at once, unless wait is set, in
// which case it retries every factsRetryInterval until a slot frees or ctx ends.
func admitFactsMaintenance(ctx context.Context, wait bool, acquire func(context.Context) (func(), error)) (func(), error) {
	for {
		release, err := acquire(ctx)
		if err == nil || !wait || !errors.Is(err, dbaccess.ErrExhausted) {
			return release, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(factsRetryInterval):
		}
	}
}

// openFactsSite resolves the site, takes admission through admit, and connects to its database.
// The caller must call release, then close the database.
func openFactsSite(ctx context.Context, name string, admit func(context.Context, *site.Config) (func(), error)) (*site.Config, *sql.DB, func(), error) {
	target, err := resolveFactsSite(ctx, name)
	if err != nil {
		return nil, nil, nil, err
	}
	siteCfg := site.ReconstructSiteConfigFromDBInfo(target, site.CommonConfigFromEnv())
	release, err := admit(ctx, siteCfg)
	if err != nil {
		return nil, nil, nil, err
	}
	database, err := site.Connect(ctx, siteCfg)
	if err != nil {
		release()
		return nil, nil, nil, fmt.Errorf("connecting to site DB: %w", err)
	}
	return siteCfg, database, release, nil
}

// resolveFactsSite looks a site up in the platform registry by name or alias.
func resolveFactsSite(ctx context.Context, name string) (site.DBSiteInfo, error) {
	startup := site.LoadStartupConfig()
	platformDB, err := dbaccess.Open(startup.DBType, startup.DBDSN)
	if err != nil {
		return site.DBSiteInfo{}, fmt.Errorf("connecting to platform DB: %w", err)
	}
	defer platformDB.Close()
	admission, err := dbaccess.AdmissionFor(startup.DBType, startup.DBDSN)
	if err != nil {
		return site.DBSiteInfo{}, fmt.Errorf("resolve platform work admission: %w", err)
	}
	var target site.DBSiteInfo
	err = runCLIWork(ctx, admission, func() error {
		id, resolveErr := site.ResolveSiteAlias(ctx, platformDB, startup.DBType, name)
		if resolveErr != nil {
			return resolveErr
		}
		target, resolveErr = site.NewSQLSiteRegistry(platformDB, startup.DBType).GetByIDContext(ctx, id)
		return resolveErr
	})
	if errors.Is(err, sql.ErrNoRows) {
		return site.DBSiteInfo{}, fmt.Errorf("site %q not found", name)
	}
	if err != nil {
		return site.DBSiteInfo{}, fmt.Errorf("resolving site %q: %w", name, err)
	}
	return target, nil
}

func loadFactsRegistry(database *sql.DB, dialect kdb.Dialect, siteName string) (*doctype.Registry, error) {
	docTypes, err := configstore.NewStore(database, dialect).LoadAll(siteName)
	if err != nil {
		return nil, fmt.Errorf("loading doctypes: %w", err)
	}
	registry := doctype.NewRegistry()
	for _, dt := range docTypes {
		registry.Register(dt)
	}
	return registry, nil
}

func formatFactsJob(job *facts.Job) string {
	return fmt.Sprintf("Job: %s\nModel: %s\nStatus: %s\nCheckpoint: %q\nProcessed: %d\nStart sequence: %d\nError: %s\n",
		job.ID, job.Model, job.Status, job.Checkpoint, job.Processed, job.StartSequence, job.Error)
}

func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
