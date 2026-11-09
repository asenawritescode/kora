package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/site"
)

func tenantSiteProvisioningAdmission(platformDriver, platformDSN string) func(context.Context, *site.Config) (func(), error) {
	platformKey := ""
	if platformDSN != "" {
		platformKey = dbaccess.ServerKey(platformDriver, platformDSN)
	}
	return func(ctx context.Context, tenant *site.Config) (func(), error) {
		if ctx == nil || tenant == nil {
			return nil, fmt.Errorf("tenant site provisioning admission requires context and configuration")
		}
		if platformKey != "" && platformKey == site.ConnectionServerKey(tenant) {
			// The console route or onboarding worker already owns the platform
			// permit for this same physical endpoint; do not acquire it twice.
			return func() {}, nil
		}
		admission, err := site.ConnectionAdmission(tenant)
		if err != nil {
			return nil, fmt.Errorf("resolve tenant provisioning admission: %w", err)
		}
		release, err := admission.AcquireMaintenance(ctx)
		if err != nil {
			if errors.Is(err, dbaccess.ErrExhausted) {
				return nil, fmt.Errorf("%w: tenant maintenance budget is exhausted", api.ErrTenantProvisioningCapacity)
			}
			return nil, fmt.Errorf("admit tenant provisioning: %w", err)
		}
		return release, nil
	}
}
