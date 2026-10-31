package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/site"
)

var siteDirectoryListCmd = &cobra.Command{
	Use:   "list",
	Short: "List credential-free site identities and runtime placement",
	Args:  cobra.NoArgs,
	RunE:  runSiteDirectoryList,
}

func runSiteDirectoryList(cmd *cobra.Command, _ []string) error {
	startup := site.LoadStartupConfig()
	if err := startup.Validate(); err != nil {
		return err
	}
	if startup.DBDSN == "" {
		return errors.New("DB_DSN is required to list the site directory")
	}
	database, err := dbaccess.Open(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("open platform site directory: %w", err)
	}
	defer database.Close()
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := func() error {
		release, err := acquireCLIWork(ctx, startup.DBType, startup.DBDSN, false)
		if err != nil {
			return err
		}
		defer release()
		return database.PingContext(ctx)
	}(); err != nil {
		return fmt.Errorf("ping platform site directory: %w", err)
	}
	registry := site.NewSQLSiteRegistry(database, startup.DBType)
	admission, err := dbaccess.AdmissionFor(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("resolve platform site directory admission: %w", err)
	}
	return writeDirectoryDescriptorPages(ctx, cmd.OutOrStdout(), registry, admission)
}

type directoryDescriptorPager interface {
	GetDirectoryDescriptorPageContext(context.Context, string, string, uint16) ([]site.SiteDescriptor, error)
}

// writeDirectoryDescriptorPages keeps memory and each admitted SQL read bounded
// while preserving the command's JSON array output shape.
func writeDirectoryDescriptorPages(ctx context.Context, output io.Writer, registry directoryDescriptorPager, admission cliWorkAdmission) error {
	const pageSize = uint16(500)
	if ctx == nil || output == nil || registry == nil || admission == nil {
		return errors.New("site directory listing requires context, output, registry, and admission")
	}
	if _, err := io.WriteString(output, "[\n"); err != nil {
		return err
	}
	after := ""
	wrote := false
	for {
		var page []site.SiteDescriptor
		err := runCLIWork(ctx, admission, func() error {
			var err error
			page, err = registry.GetDirectoryDescriptorPageContext(ctx, "", after, pageSize)
			return err
		})
		if err != nil {
			return fmt.Errorf("read site directory page after %q: %w", after, err)
		}
		for _, descriptor := range page {
			encoded, err := json.MarshalIndent(descriptor, "", "  ")
			if err != nil {
				return fmt.Errorf("encode site directory descriptor: %w", err)
			}
			if wrote {
				if _, err := io.WriteString(output, ",\n"); err != nil {
					return err
				}
			} else {
				wrote = true
			}
			if _, err := io.WriteString(output, "  "+strings.ReplaceAll(string(encoded), "\n", "\n  ")); err != nil {
				return err
			}
			after = descriptor.SiteID
		}
		if len(page) < int(pageSize) {
			break
		}
	}
	if wrote {
		if _, err := io.WriteString(output, "\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(output, "]\n")
	return err
}
