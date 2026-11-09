package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/internal/natsprovider"
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

func natsEnabled() bool {
	v := os.Getenv("KORA_EVENT_PROVIDER")
	return v == "nats"
}
