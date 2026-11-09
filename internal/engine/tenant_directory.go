package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/contract"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
)

// TenantDirectory is the Engine-owned directory read/change-feed boundary used
// to activate local tenant runtimes from durable platform state.
type TenantDirectory interface {
	site.DirectoryChangeFeed
	GetDescriptorByID(string) (site.Descriptor, error)
	GetByID(string) (site.DBSiteInfo, error)
	GetDirectoryDescriptors() ([]contract.SiteDescriptor, error)
	GetDirectorySnapshotForCell(string) ([]site.DBSiteInfo, error)
}

// TenantRuntimeDirectory is the startup/reload directory contract used by the
// runtime assembly and administrative reload service.
type TenantRuntimeDirectory interface {
	TenantDirectory
	CurrentDirectoryRevision() (uint64, error)
	GetByIDContext(context.Context, string) (site.DBSiteInfo, error)
	GetDirectoryDescriptorPageContext(context.Context, string, string, uint16) ([]site.Descriptor, error)
}

// TenantIngressDirectory is the credential-free placement projection consumed
// by optional deployment ingress. It never owns tenant data or routing state.
type TenantIngressDirectory interface {
	ValidateDirectory([]contract.SiteDescriptor) error
	ApplyDescriptor(contract.SiteDescriptor) error
	MarkLocalReady(string, bool)
	ReplaceDirectory([]contract.SiteDescriptor) error
}

// TenantDirectoryConfig injects the process-specific runtime service installer
// through one Engine runtime coordinator while keeping directory policy inside
// Engine.
type TenantDirectoryConfig struct {
	Directory     TenantDirectory
	Tenants       *knet.SiteRouter
	CellID        string
	ConsumerID    string
	InitialCursor uint64
	Coordinator   *TenantRuntimeCoordinator
	WorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
}

// NewTenantDirectoryConsumer builds the durable tenant activation policy used
// by both feed polling and snapshot resync. It updates directory revision
// metadata even when config revision is unchanged and delegates construction of
// non-core sidecars to the process composition root.
func NewTenantDirectoryConsumer(config TenantDirectoryConfig) (*site.DirectoryConsumer, error) {
	if config.Directory == nil || config.Tenants == nil || config.Coordinator == nil || config.ConsumerID == "" {
		return nil, errors.New("tenant directory, runtime manager, coordinator, and consumer identity are required")
	}
	if config.Coordinator.tenants != config.Tenants {
		return nil, errors.New("tenant directory and runtime coordinator must use the same manager")
	}
	config.Tenants.SetLazyActivator(config.Coordinator.ActivateFromDirectory)
	ingress := config.Coordinator.ingress
	consumer := &site.DirectoryConsumer{
		Registry: config.Directory, ID: config.ConsumerID, PageSize: 100, InitialCursor: config.InitialCursor,
		WorkAdmission: config.WorkAdmission,
	}
	if contextRegistry, ok := config.Directory.(site.ContextDirectoryChangeFeed); ok {
		consumer.ContextRegistry = contextRegistry
	}
	consumer.Apply = func(ctx context.Context, change site.DirectoryChange) error {
		applyIngress := func(descriptor contract.SiteDescriptor) error {
			if ingress == nil {
				return nil
			}
			return ingress.ApplyDescriptor(descriptor)
		}
		descriptor := change.Descriptor
		directoryRevision := change.Cursor
		if config.CellID != "" && descriptor.Status != "deleted" {
			current, err := config.Directory.GetDescriptorByID(descriptor.SiteID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					if err := applyIngress(contract.SiteDescriptor{SiteID: descriptor.SiteID, Status: "deleted"}); err != nil {
						return fmt.Errorf("remove tenant %s from ingress directory: %w", descriptor.SiteID, err)
					}
					return config.Coordinator.Retire(ctx, descriptor.SiteID)
				}
				return fmt.Errorf("read current placement for tenant %s: %w", descriptor.SiteID, err)
			}
			current.DirectoryRevision = directoryRevision
			descriptor = current
		}
		if err := site.ValidateDeploymentDirectoryV2([]site.Descriptor{descriptor}, directoryRevision); err != nil {
			return fmt.Errorf("validating deployment assignment for tenant directory change %s: %w", descriptor.SiteID, err)
		}
		localAssignment := descriptor.Status != "deleted" && site.BelongsToDeployment(descriptor.RuntimeCellID, config.CellID)
		if deploymentID := strings.TrimSpace(descriptor.RuntimeCellID); deploymentID != "" && deploymentID != "default" {
			slog.Info("applying nondefault tenant deployment assignment",
				"site_id", descriptor.SiteID, "deployment_id", deploymentID,
				"local_deployment_id", config.CellID, "local_owner", localAssignment,
				"config_revision", descriptor.ConfigRevision, "directory_revision", directoryRevision)
		}
		if !localAssignment {
			config.Tenants.RemoveLazyDescriptor(descriptor.SiteID)
		} else {
			config.Tenants.UpdateLazyDescriptor(descriptor)
		}
		if descriptor.Status == "deleted" {
			if err := applyIngress(descriptor); err != nil {
				return fmt.Errorf("apply tenant %s to ingress directory: %w", descriptor.SiteID, err)
			}
			return config.Coordinator.Retire(ctx, descriptor.SiteID)
		}
		if !localAssignment {
			if err := applyIngress(descriptor); err != nil {
				return fmt.Errorf("apply tenant %s to ingress directory: %w", descriptor.SiteID, err)
			}
			return config.Coordinator.Retire(ctx, descriptor.SiteID)
		}
		if descriptor.Status != "active" && descriptor.Status != "degraded" {
			if config.Tenants.SnapshotByID(descriptor.SiteID) != nil {
				if err := config.Tenants.ApplySiteDescriptor(descriptor); err != nil {
					return err
				}
				if err := config.Coordinator.Retire(ctx, descriptor.SiteID); err != nil {
					return err
				}
			}
			if ingress != nil {
				if err := applyIngress(descriptor); err != nil {
					return fmt.Errorf("apply tenant %s to ingress directory: %w", descriptor.SiteID, err)
				}
				ingress.MarkLocalReady(descriptor.SiteID, false)
			}
			return nil
		}

		if loaded := config.Tenants.SnapshotByID(descriptor.SiteID); loaded != nil {
			if loaded.ConfigRevision >= descriptor.ConfigRevision {
				if loaded.ConfigRevision == descriptor.ConfigRevision {
					descriptor.DirectoryRevision = directoryRevision
					if err := config.Tenants.ApplySiteDescriptor(descriptor); err != nil {
						return fmt.Errorf("apply tenant directory projection %s: %w", descriptor.SiteID, err)
					}
				} else if directoryRevision > loaded.Activation.DirectoryRevision {
					if err := config.Tenants.AdvanceDirectoryRevision(descriptor.SiteID, directoryRevision); err != nil {
						return err
					}
				}
				if ingress != nil {
					if err := applyIngress(descriptor); err != nil {
						return fmt.Errorf("apply tenant %s to ingress directory: %w", descriptor.SiteID, err)
					}
					ingress.MarkLocalReady(descriptor.SiteID, true)
				}
				return nil
			}
			info, err := config.Directory.GetByID(descriptor.SiteID)
			if err != nil {
				return fmt.Errorf("load newer tenant revision %s: %w", descriptor.SiteID, err)
			}
			if err := config.Coordinator.Replace(ctx, info, directoryRevision); err != nil {
				return err
			}
			if ingress != nil {
				if err := applyIngress(descriptor); err != nil {
					return fmt.Errorf("apply tenant %s to ingress directory: %w", descriptor.SiteID, err)
				}
				ingress.MarkLocalReady(descriptor.SiteID, true)
			}
			return nil
		}
		if ingress != nil {
			ingress.MarkLocalReady(descriptor.SiteID, true)
			if err := applyIngress(descriptor); err != nil {
				return fmt.Errorf("apply tenant %s to ingress directory: %w", descriptor.SiteID, err)
			}
		}
		return nil
	}
	consumer.Resync = func(ctx context.Context, directoryRevision uint64) error {
		descriptors, err := config.Directory.GetDirectoryDescriptors()
		if err != nil {
			return fmt.Errorf("read credential-free tenant directory snapshot: %w", err)
		}
		if err := site.ValidateDeploymentDirectoryV2(descriptors, directoryRevision); err != nil {
			return fmt.Errorf("validate tenant deployment assignments during directory resync: %w", err)
		}
		localDescriptors := make([]contract.SiteDescriptor, 0, len(descriptors))
		for _, descriptor := range descriptors {
			if site.BelongsToDeployment(descriptor.RuntimeCellID, config.CellID) {
				localDescriptors = append(localDescriptors, descriptor)
			}
		}
		config.Tenants.SetLazyDirectory(localDescriptors, config.Coordinator.ActivateFromDirectory)
		if ingress != nil {
			if err := ingress.ValidateDirectory(descriptors); err != nil {
				return fmt.Errorf("validate ingress placement during directory resync: %w", err)
			}
			if err := ingress.ReplaceDirectory(descriptors); err != nil {
				return fmt.Errorf("replace ingress directory snapshot: %w", err)
			}
			for _, descriptor := range descriptors {
				ready := site.BelongsToDeployment(descriptor.RuntimeCellID, config.CellID) && (descriptor.Status == "active" || descriptor.Status == "degraded")
				ingress.MarkLocalReady(descriptor.SiteID, ready)
			}
		}
		present := make(map[string]struct{}, len(localDescriptors))
		for _, descriptor := range localDescriptors {
			if descriptor.SiteID == "" {
				continue
			}
			present[descriptor.SiteID] = struct{}{}
			loaded := config.Tenants.SnapshotByID(descriptor.SiteID)
			if descriptor.Status == "deleted" {
				if loaded != nil {
					if err := config.Coordinator.Retire(ctx, descriptor.SiteID); err != nil {
						return fmt.Errorf("retire deleted tenant %s during resync: %w", descriptor.SiteID, err)
					}
				}
				continue
			}
			if descriptor.Status != "active" && descriptor.Status != "degraded" {
				if loaded != nil {
					if err := config.Tenants.ApplySiteDescriptor(descriptor); err != nil {
						return err
					}
					if err := config.Coordinator.Retire(ctx, descriptor.SiteID); err != nil {
						return fmt.Errorf("retire inactive tenant %s during resync: %w", descriptor.SiteID, err)
					}
				}
				continue
			}
			if loaded != nil {
				if loaded.ConfigRevision >= descriptor.ConfigRevision {
					if loaded.ConfigRevision == descriptor.ConfigRevision {
						descriptor.DirectoryRevision = directoryRevision
						if err := config.Tenants.ApplySiteDescriptor(descriptor); err != nil {
							return err
						}
					} else if err := config.Tenants.AdvanceDirectoryRevision(descriptor.SiteID, directoryRevision); err != nil {
						return err
					}
					continue
				}
				info, err := config.Directory.GetByID(descriptor.SiteID)
				if err != nil {
					return fmt.Errorf("load updated active tenant %s during resync: %w", descriptor.SiteID, err)
				}
				if err := config.Coordinator.Replace(ctx, info, directoryRevision); err != nil {
					return err
				}
				continue
			}
		}
		for _, loaded := range config.Tenants.AllSites() {
			if loaded.SiteID == "" {
				continue
			}
			if _, ok := present[loaded.SiteID]; ok {
				continue
			}
			if err := config.Coordinator.Retire(ctx, loaded.SiteID); err != nil {
				return err
			}
		}
		return nil
	}
	return consumer, nil
}
