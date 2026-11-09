package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/site"
)

const maxDirectoryAssignmentBuckets = 4096

type directoryAssignmentBucket struct {
	DeploymentID string `json:"deployment_id"`
	Status       string `json:"status"`
	Count        uint64 `json:"count"`
}

type directoryAssignmentKey struct {
	deploymentID string
	status       string
}

// directoryAssignmentSummary deliberately omits tenant identifiers, aliases,
// database settings, and credentials. RuntimeCellID is named as a v1 source
// field because it remains the single persisted placement authority today.
type directoryAssignmentSummary struct {
	Version           int                         `json:"version"`
	PlacementSource   string                      `json:"placement_source"`
	DirectoryRevision uint64                      `json:"directory_revision"`
	TotalEntries      uint64                      `json:"total_entries"`
	Assignments       []directoryAssignmentBucket `json:"assignments"`
}

type directoryAssignmentPager interface {
	GetDirectoryDescriptorPageContext(context.Context, string, string, uint16) ([]site.Descriptor, error)
	CurrentDirectoryRevisionContext(context.Context) (uint64, error)
}

var siteDirectoryAssignmentSummaryCmd = &cobra.Command{
	Use:   "summarize-assignments",
	Short: "Summarize site placement counts without tenant identifiers",
	Long:  "Read the authoritative site directory in bounded pages and emit counts by normalized placement and status. The output excludes tenant IDs, aliases, database settings, and credentials.",
	Args:  cobra.NoArgs,
	RunE:  runSiteDirectoryAssignmentSummary,
}

func runSiteDirectoryAssignmentSummary(cmd *cobra.Command, _ []string) error {
	startup := site.LoadStartupConfig()
	if err := startup.Validate(); err != nil {
		return err
	}
	if startup.DBDSN == "" {
		return errors.New("DB_DSN is required to summarize site assignments")
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
	admission, err := dbaccess.AdmissionFor(startup.DBType, startup.DBDSN)
	if err != nil {
		return fmt.Errorf("resolve platform site directory admission: %w", err)
	}
	if err := runCLIWork(ctx, admission, func() error { return database.PingContext(ctx) }); err != nil {
		return fmt.Errorf("ping platform site directory: %w", err)
	}
	registry := site.NewSQLSiteRegistry(database, startup.DBType)
	summary, err := summarizeDirectoryAssignments(ctx, registry, admission)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func summarizeDirectoryAssignments(ctx context.Context, registry directoryAssignmentPager, admission cliWorkAdmission) (directoryAssignmentSummary, error) {
	const pageSize = uint16(500)
	if ctx == nil || registry == nil || admission == nil {
		return directoryAssignmentSummary{}, errors.New("assignment summary requires context, registry, and admission")
	}
	readRevision := func() (uint64, error) {
		var revision uint64
		err := runCLIWork(ctx, admission, func() error {
			var err error
			revision, err = registry.CurrentDirectoryRevisionContext(ctx)
			return err
		})
		return revision, err
	}
	startRevision, err := readRevision()
	if err != nil {
		return directoryAssignmentSummary{}, fmt.Errorf("read site directory revision before assignment inventory: %w", err)
	}
	counts := make(map[directoryAssignmentKey]directoryAssignmentBucket)
	var total uint64
	after := ""
	for {
		var page []site.Descriptor
		err := runCLIWork(ctx, admission, func() error {
			var err error
			page, err = registry.GetDirectoryDescriptorPageContext(ctx, "", after, pageSize)
			return err
		})
		if err != nil {
			return directoryAssignmentSummary{}, fmt.Errorf("read site directory assignment page after %q: %w", after, err)
		}
		if len(page) > int(pageSize) {
			return directoryAssignmentSummary{}, errors.New("site directory returned an oversized assignment page")
		}
		previous := after
		for _, descriptor := range page {
			if descriptor.SiteID == "" || descriptor.SiteID <= previous {
				return directoryAssignmentSummary{}, errors.New("site directory assignment page is not strictly ordered")
			}
			previous = descriptor.SiteID
			placement, err := site.NormalizeRuntimeCellID(descriptor.RuntimeCellID)
			if err != nil {
				return directoryAssignmentSummary{}, fmt.Errorf("site directory contains invalid placement metadata: %w", err)
			}
			if placement == "" {
				placement = "default"
			}
			status := strings.ToLower(strings.TrimSpace(descriptor.Status))
			if status == "" || len(status) > 32 {
				return directoryAssignmentSummary{}, errors.New("site directory contains invalid status metadata")
			}
			key := directoryAssignmentKey{deploymentID: placement, status: status}
			bucket, exists := counts[key]
			if !exists {
				if len(counts) >= maxDirectoryAssignmentBuckets {
					return directoryAssignmentSummary{}, fmt.Errorf("assignment summary exceeds %d distinct placement/status buckets", maxDirectoryAssignmentBuckets)
				}
				bucket = directoryAssignmentBucket{DeploymentID: placement, Status: status}
			}
			bucket.Count++
			counts[key] = bucket
			total++
		}
		after = previous
		if len(page) < int(pageSize) {
			break
		}
	}
	endRevision, err := readRevision()
	if err != nil {
		return directoryAssignmentSummary{}, fmt.Errorf("read site directory revision after assignment inventory: %w", err)
	}
	if startRevision != endRevision {
		return directoryAssignmentSummary{}, fmt.Errorf("site directory changed during assignment inventory (revision %d to %d); rerun the summary", startRevision, endRevision)
	}
	assignments := make([]directoryAssignmentBucket, 0, len(counts))
	for _, bucket := range counts {
		assignments = append(assignments, bucket)
	}
	sortDirectoryAssignmentBuckets(assignments)
	if assignments == nil {
		assignments = []directoryAssignmentBucket{}
	}
	return directoryAssignmentSummary{
		Version: 1, PlacementSource: "runtime_cell_id", DirectoryRevision: startRevision,
		TotalEntries: total, Assignments: assignments,
	}, nil
}

func sortDirectoryAssignmentBuckets(assignments []directoryAssignmentBucket) {
	sort.Slice(assignments, func(i, j int) bool {
		if assignments[i].DeploymentID == assignments[j].DeploymentID {
			return assignments[i].Status < assignments[j].Status
		}
		return assignments[i].DeploymentID < assignments[j].DeploymentID
	})
}
