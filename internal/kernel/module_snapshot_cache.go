package kernel

import (
	"sync"

	"github.com/asenawritescode/kora/contract"
)

// moduleSnapshotCache keeps each tenant's decoded module snapshot together
// with the row revision it was decoded from. A revision is written only with
// the snapshot_json that carries it, so an equal revision means equal content:
// writers that read the same revision skip the JSON decode.
type moduleSnapshotCache struct {
	mu       sync.Mutex
	byTenant map[contract.TenantID]cachedModuleSnapshot
}

type cachedModuleSnapshot struct {
	revision int64
	snapshot *contract.ModuleSnapshot
}

func (c *moduleSnapshotCache) lookup(tenant contract.TenantID, revision int64) (*contract.ModuleSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byTenant[tenant]
	if !ok || entry.revision != revision {
		return nil, false
	}
	return entry.snapshot, true
}

func (c *moduleSnapshotCache) store(tenant contract.TenantID, revision int64, snapshot *contract.ModuleSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byTenant == nil {
		c.byTenant = map[contract.TenantID]cachedModuleSnapshot{}
	}
	c.byTenant[tenant] = cachedModuleSnapshot{revision: revision, snapshot: snapshot}
}
