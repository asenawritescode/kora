package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/ingress"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/site"
)

var siteDirectoryAssignCellApply bool
var siteDirectoryAssignDeploymentApply bool

var siteDirectoryAssignCellCmd = &cobra.Command{
	Use:   "assign-cell <site-id> <cell-id>",
	Short: "Preview or change a site's legacy Engine cell assignment",
	Long: `Compatibility command for the deployment-named placement command. Preview a
site placement change, or add --apply to write a revisioned directory change.
Before applying, KORA_ENGINE_CELL_URLS must include the target cell's direct
Engine origin. Keep this command while existing automation uses the legacy name.`,
	Args: cobra.ExactArgs(2),
	RunE: runSiteDirectoryAssignCell,
}

var siteDirectoryAssignDeploymentCmd = &cobra.Command{
	Use:   "assign-deployment <site-id> <deployment-id>",
	Short: "Preview or change a site's Engine deployment assignment",
	Long: `Preview a site placement change. Add --apply to write a revisioned directory
change. Before applying, KORA_ENGINE_DEPLOYMENT_URLS must include the target
deployment's direct Engine origin. This updates the existing canonical
assignment through the compatibility adapter; it does not perform traffic
cutover or replace deployment ingress.`,
	Args: cobra.ExactArgs(2),
	RunE: runSiteDirectoryAssignDeployment,
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
	GetDescriptorByIDContext(ctx context.Context, siteID string) (site.SiteDescriptor, error)
	SetRuntimeCellIDAtRevisionContext(ctx context.Context, siteID, cellID string, expectedRevision uint64) (site.SiteDescriptor, error)
}

func runSiteDirectoryAssignCell(cmd *cobra.Command, args []string) error {
	return runSiteDirectoryAssignment(cmd, args, siteDirectoryAssignCellApply, "cell", "KORA_ENGINE_CELL_URLS", ingress.ParseCellURLs, writeSiteCellAssignmentReport)
}

type siteDeploymentAssignmentReport struct {
	SiteID             string   `json:"site_id"`
	Aliases            []string `json:"aliases"`
	Status             string   `json:"status"`
	PreviousDeployment string   `json:"previous_deployment_id,omitempty"`
	Deployment         string   `json:"deployment_id"`
	ConfigRevision     uint64   `json:"config_revision,omitempty"`
	Applied            bool     `json:"applied"`
}

func runSiteDirectoryAssignDeployment(cmd *cobra.Command, args []string) error {
	return runSiteDirectoryAssignment(cmd, args, siteDirectoryAssignDeploymentApply, "deployment", "KORA_ENGINE_DEPLOYMENT_URLS", ingress.ParseDeploymentURLs, writeSiteDeploymentAssignmentReport)
}

type assignmentURLParser func(string) (map[string]string, error)
type assignmentReportWriter func(*cobra.Command, siteCellAssignmentReport) error

func runSiteDirectoryAssignment(cmd *cobra.Command, args []string, apply bool, placement, endpointEnv string, parseURLs assignmentURLParser, writeReport assignmentReportWriter) error {
	siteID := strings.TrimSpace(args[0])
	targetID, err := site.NormalizeRuntimeCellID(args[1])
	if err != nil || targetID == "" {
		return fmt.Errorf("%s id is required and may contain only letters, numbers, dot, dash, and underscore", placement)
	}
	if siteID == "" {
		return errors.New("site id is required")
	}

	startup := site.LoadStartupConfig()
	if err := startup.Validate(); err != nil {
		return err
	}
	if startup.DBDSN == "" {
		return fmt.Errorf("DB_DSN is required to assign an Engine %s", placement)
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
	releasePing, err := acquireCLIWork(ctx, startup.DBType, startup.DBDSN, false)
	if err != nil {
		return err
	}
	pingErr := database.PingContext(ctx)
	releasePing()
	if pingErr != nil {
		return fmt.Errorf("ping platform site directory: %w", pingErr)
	}
	registry := site.NewSQLSiteRegistry(database, startup.DBType)
	var endpoints map[string]string
	if apply {
		endpoints, err = parseURLs(os.Getenv(endpointEnv))
		if err != nil {
			return fmt.Errorf("validate %s: %w", endpointEnv, err)
		}
	}
	admission, err := dbaccess.AdmissionFor(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("resolve site assignment admission: %w", err)
	}
	var report siteCellAssignmentReport
	err = runCLIWork(ctx, admission, func() error {
		var operationErr error
		report, operationErr = applySiteCellAssignment(ctx, registry, siteID, targetID, apply, endpoints, placement)
		return operationErr
	})
	if err != nil {
		return err
	}
	return writeReport(cmd, report)
}

func writeSiteDeploymentAssignmentReport(cmd *cobra.Command, report siteCellAssignmentReport) error {
	return writeAssignmentJSON(cmd, siteDeploymentAssignmentReport{
		SiteID: report.SiteID, Aliases: report.Aliases, Status: report.Status,
		PreviousDeployment: report.PreviousRuntimeCell, Deployment: report.RuntimeCell,
		ConfigRevision: report.ConfigRevision, Applied: report.Applied,
	})
}

func applySiteCellAssignment(ctx context.Context, registry siteCellAssignmentRegistry, siteID, targetCell string, apply bool, cellURLs map[string]string, placement string) (siteCellAssignmentReport, error) {
	if ctx == nil {
		return siteCellAssignmentReport{}, errors.New("site placement update requires a context")
	}
	descriptor, err := registry.GetDescriptorByIDContext(ctx, siteID)
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
		return siteCellAssignmentReport{}, fmt.Errorf("refusing assignment: no direct Engine origin is configured for target %s %q", placement, targetCell)
	}
	updated, err := registry.SetRuntimeCellIDAtRevisionContext(ctx, siteID, targetCell, descriptor.ConfigRevision)
	if err != nil {
		return siteCellAssignmentReport{}, fmt.Errorf("assign site %s to Engine %s %s: %w", siteID, placement, targetCell, err)
	}
	report.Aliases = updated.Aliases
	report.Status = updated.Status
	report.RuntimeCell = updated.RuntimeCellID
	report.ConfigRevision = updated.ConfigRevision
	report.Applied = true
	return report, nil
}

func writeSiteCellAssignmentReport(cmd *cobra.Command, report siteCellAssignmentReport) error {
	return writeAssignmentJSON(cmd, report)
}

func writeAssignmentJSON(cmd *cobra.Command, report any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func init() {
	siteDirectoryAssignCellCmd.Flags().BoolVar(&siteDirectoryAssignCellApply, "apply", false, "persist this placement change as a revisioned directory update")
	siteDirectoryAssignDeploymentCmd.Flags().BoolVar(&siteDirectoryAssignDeploymentApply, "apply", false, "persist this placement change as a revisioned directory update")
}
