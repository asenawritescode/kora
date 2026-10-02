package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

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
	database, err := sql.Open(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("open platform site directory: %w", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		return fmt.Errorf("ping platform site directory: %w", err)
	}
	descriptors, err := site.NewSQLSiteRegistry(database, startup.DBType).GetDirectoryDescriptors()
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(descriptors)
}
