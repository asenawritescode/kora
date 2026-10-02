package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/ingress"
	"github.com/asenawritescode/kora/site"
)

var siteDirectoryAssignCellApply bool

var siteDirectoryAssignCellCmd = &cobra.Command{
	Use:   "assign-cell <site-id> <cell-id>",
	Short: "Preview or change a site's eager Engine cell assignment",
	Long: `Preview a site placement change. Add --apply to write a revisioned directory
change. Before applying, KORA_ENGINE_CELL_URLS must include the target cell's
direct Engine origin. Every Engine cell's ingress configuration must also know
the target endpoint before traffic is switched to it. The assigned runtime is
eagerly loaded by its destination cell; it is never initialized on first request.`,
	Args: cobra.ExactArgs(2),
	RunE: runSiteDirectoryAssignCell,
}

type siteCellAssignmentReport struct {
	SiteID              string   `json:"site_id"`
	Aliases             []string `json:"aliases"`
	Status              string   `json:"status"`
	PreviousRuntimeCell string   `json:"previous_runtime_cell_id,omitempty"`
	RuntimeCell         string   `json:"runtime_cell_id"`
	ConfigRevision      uint64   `json:"config_revision,omitempty"`
	Applied             bool     `json:"applied"`
}

type siteCellAssignmentRegistry interface {
	GetDescriptorByID(siteID string) (site.SiteDescriptor, error)
	SetRuntimeCellID(siteID, cellID string) (site.SiteDescriptor, error)
}

func runSiteDirectoryAssignCell(cmd *cobra.Command, args []string) error {
	siteID := strings.TrimSpace(args[0])
	targetCell, err := site.NormalizeRuntimeCellID(args[1])
	if err != nil || targetCell == "" {
		return errors.New("cell id is required and may contain only letters, numbers, dot, dash, and underscore")
	}
	if siteID == "" {
		return errors.New("site id is required")
	}

	startup := site.LoadStartupConfig()
	if err := startup.Validate(); err != nil {
		return err
	}
	if startup.DBDSN == "" {
		return errors.New("DB_DSN is required to assign an Engine cell")
	}
	database, err := sql.Open(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("open platform site directory: %w", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		return fmt.Errorf("ping platform site directory: %w", err)
	}
	registry := site.NewSQLSiteRegistry(database, startup.DBType)
	var cellURLs map[string]string
	if siteDirectoryAssignCellApply {
		cellURLs, err = ingress.ParseCellURLs(os.Getenv("KORA_ENGINE_CELL_URLS"))
		if err != nil {
			return fmt.Errorf("validate KORA_ENGINE_CELL_URLS: %w", err)
		}
	}
	report, err := applySiteCellAssignment(registry, siteID, targetCell, siteDirectoryAssignCellApply, cellURLs)
	if err != nil {
		return err
	}
	return writeSiteCellAssignmentReport(cmd, report)
}

func applySiteCellAssignment(registry siteCellAssignmentRegistry, siteID, targetCell string, apply bool, cellURLs map[string]string) (siteCellAssignmentReport, error) {
	descriptor, err := registry.GetDescriptorByID(siteID)
	if err != nil {
		return siteCellAssignmentReport{}, fmt.Errorf("read site %s: %w", siteID, err)
	}
	previousCell := strings.TrimSpace(descriptor.RuntimeCellID)
	report := siteCellAssignmentReport{
		SiteID: siteID, Aliases: descriptor.Aliases, Status: descriptor.Status,
		PreviousRuntimeCell: previousCell, RuntimeCell: targetCell,
		ConfigRevision: descriptor.ConfigRevision,
	}
	if previousCell == targetCell || (previousCell == "" && targetCell == "default") || !apply {
		return report, nil
	}
	if _, exists := cellURLs[targetCell]; !exists {
		return siteCellAssignmentReport{}, fmt.Errorf("refusing assignment: KORA_ENGINE_CELL_URLS has no direct Engine origin for target cell %q", targetCell)
	}
	updated, err := registry.SetRuntimeCellID(siteID, targetCell)
	if err != nil {
		return siteCellAssignmentReport{}, fmt.Errorf("assign site %s to Engine cell %s: %w", siteID, targetCell, err)
	}
	report.Aliases = updated.Aliases
	report.Status = updated.Status
	report.RuntimeCell = updated.RuntimeCellID
	report.ConfigRevision = updated.ConfigRevision
	report.Applied = true
	return report, nil
}

func writeSiteCellAssignmentReport(cmd *cobra.Command, report siteCellAssignmentReport) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func init() {
	siteDirectoryAssignCellCmd.Flags().BoolVar(&siteDirectoryAssignCellApply, "apply", false, "persist this placement change as a revisioned directory update")
}
