package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/natsprovider"
	"github.com/asenawritescode/kora/internal/site"
)

var natsCmd = &cobra.Command{
	Use:   "nats",
	Short: "Manage self-hosted NATS resources",
}

var natsValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate NATS connectivity and stream bootstrap",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := connectNATSProvider()
		if err != nil {
			return err
		}
		defer p.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.Bootstrap(ctx); err != nil {
			return err
		}
		fmt.Println("NATS validation succeeded")
		return nil
	},
}

var natsBootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Idempotently create or validate Kigae NATS resources",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := connectNATSProvider()
		if err != nil {
			return err
		}
		defer p.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.Bootstrap(ctx); err != nil {
			return err
		}
		fmt.Println("NATS bootstrap complete")
		return nil
	},
}

var natsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show NATS provider status",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := natsprovider.FromEnv()
		if err := cfg.Validate(); err != nil {
			return err
		}
		manifest := cfg.DeploymentManifest()
		manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
		fmt.Printf("name=%s\nservers=%v\nstream=%s\nsubject_prefix=%s\n%s\n", cfg.Name, cfg.SortedServerURLs(), cfg.StreamName, cfg.SubjectPrefix, string(manifestJSON))
		return nil
	},
}

var natsBackupManifestCmd = &cobra.Command{
	Use:   "backup-manifest",
	Short: "Print the RFC deployment manifest for backup/restore operations",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := natsprovider.FromEnv()
		if err := cfg.Validate(); err != nil {
			return err
		}
		manifest := cfg.DeploymentManifest()
		enc, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(enc))
		return nil
	},
}

var natsDrainCmd = &cobra.Command{
	Use:   "drain",
	Short: "Drain the configured NATS connection and exit",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := connectNATSProvider()
		if err != nil {
			return err
		}
		defer p.Close()
		fmt.Println("NATS drain complete")
		return nil
	},
}

func connectNATSProvider() (*natsprovider.Provider, error) {
	cfg := natsprovider.FromEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return natsprovider.New(context.Background(), cfg)
}

func init() {
	natsCmd.AddCommand(natsValidateCmd)
	natsCmd.AddCommand(natsBootstrapCmd)
	natsCmd.AddCommand(natsStatusCmd)
	natsCmd.AddCommand(natsBackupManifestCmd)
	natsCmd.AddCommand(natsDrainCmd)
	rootCmd.AddCommand(natsCmd)

	for _, c := range []*cobra.Command{natsValidateCmd, natsBootstrapCmd, natsStatusCmd, natsBackupManifestCmd, natsDrainCmd} {
		c.Flags().String("site", "", "Reserved for site-scoped bootstrap flows")
		_ = c.Flags().MarkHidden("site")
	}
}

// announceDirectoryRevision wakes running Engines after this CLI process
// committed a site directory change. It skips quietly when NATS is not the
// event provider, and logs a failed announce: the Engine recovers through its
// NATS-down revision check or 5 min sweep.
func announceDirectoryRevision(driver, dsn string) {
	if !natsEnabled() {
		return
	}
	database, err := dbaccess.Open(driver, dsn)
	if err != nil {
		slog.Warn("announce site directory change: open platform registry failed", "error", err)
		return
	}
	defer database.Close()
	provider, err := connectNATSProvider()
	if err != nil {
		slog.Warn("announce site directory change: NATS unavailable; Engine recovers on its sweep", "error", err)
		return
	}
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := site.AnnounceDirectoryRevision(ctx, database, provider); err != nil {
		slog.Warn("announce site directory change failed; Engine recovers on its sweep", "error", err)
	}
}

func natsEnabled() bool {
	v := os.Getenv("KORA_EVENT_PROVIDER")
	return v == "nats"
}
