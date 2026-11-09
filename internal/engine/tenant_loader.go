package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// LoadTenantRuntime prepares a tenant generation's database-backed registry
// before it is published to the runtime manager. A compatible previous
// generation may lend its stable DB and session manager; failed preparation
// never closes those borrowed resources.
func LoadTenantRuntime(ctx context.Context, info site.DBSiteInfo, common *site.CommonConfig, reusable *knet.LoadedSite, binaryVersion string) (runtime *knet.LoadedSite, err error) {
	return loadTenantRuntime(ctx, info, common, reusable, binaryVersion, tenantRuntimeDependencies{
		connect:    site.Connect,
		bootstrap:  site.BootstrapSystemTables,
		claimOwner: site.ClaimDatabaseOwner,
	})
}

type tenantRuntimeDependencies struct {
	connect    func(*site.Config) (*sql.DB, error)
	bootstrap  func(*sql.DB, kdb.Dialect) error
	claimOwner func(context.Context, *sql.DB, kdb.Dialect, string) error
}

func loadTenantRuntime(ctx context.Context, info site.DBSiteInfo, common *site.CommonConfig, reusable *knet.LoadedSite, binaryVersion string, dependencies tenantRuntimeDependencies) (runtime *knet.LoadedSite, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dependencies.connect == nil {
		dependencies.connect = site.Connect
	}
	if dependencies.bootstrap == nil {
		dependencies.bootstrap = site.BootstrapSystemTables
	}
	tracer := otel.Tracer("kora/engine/tenant_runtime")
	ctx, span := tracer.Start(ctx, "engine.load_tenant_runtime", trace.WithAttributes(attribute.String("kora.site.name", info.Name)))
	defer span.End()
	siteCfg := site.ReconstructSiteConfigFromDBInfo(info, common)
	dialect := kdb.Resolve(siteCfg.DBType)
	databaseIdentity := site.ConnectionIdentity(siteCfg)
	span.SetAttributes(attribute.String("db.system", siteCfg.DBType))
	var database *sql.DB
	var sessionManager *auth.SessionManager
	ownsDatabase := true
	if canReuseTenantResources(reusable, databaseIdentity) {
		database = reusable.DB
		sessionManager = reusable.SessionManager
		ownsDatabase = false
	} else {
		_, phase := tracer.Start(ctx, "engine.tenant.connect")
		database, err = dependencies.connect(siteCfg)
		if err != nil {
			phase.SetStatus(codes.Error, "tenant database connection failed")
		}
		phase.End()
		if err != nil {
			return nil, fmt.Errorf("connecting to database: %w", err)
		}
	}
	transferred := false
	defer func() {
		if transferred || !ownsDatabase || database == nil || err == nil {
			return
		}
		if closeErr := database.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close tenant database after activation failure: %w", closeErr))
		}
	}()
	_, phase := tracer.Start(ctx, "engine.tenant.bootstrap")
	err = dependencies.bootstrap(database, dialect)
	if err != nil {
		phase.SetStatus(codes.Error, "tenant database bootstrap failed")
	}
	phase.End()
	if err != nil {
		return nil, fmt.Errorf("bootstrapping system tables: %w", err)
	}
	// LibSQL tenants all resolve to the shared DB_DSN database and its business
	// tables are not tenant-prefixed, so only one tenant may own it.
	if siteCfg.DBType == "libsql" && dependencies.claimOwner != nil {
		if err = dependencies.claimOwner(ctx, database, dialect, info.SiteID); err != nil {
			return nil, fmt.Errorf("claiming tenant database: %w", err)
		}
	}
	_, phase = tracer.Start(ctx, "engine.tenant.load_config")
	store := configstore.NewStore(database, dialect)
	doctypes, err := store.LoadAll(info.Name)
	if err != nil {
		phase.SetStatus(codes.Error, "tenant configuration load failed")
		phase.End()
		return nil, fmt.Errorf("loading doctypes: %w", err)
	}
	roles, err := store.LoadRoles(info.Name)
	if err != nil {
		phase.SetStatus(codes.Error, "tenant configuration load failed")
		phase.End()
		return nil, fmt.Errorf("loading roles: %w", err)
	}
	permissions, err := store.LoadPermissions(info.Name)
	if err != nil {
		phase.SetStatus(codes.Error, "tenant configuration load failed")
		phase.End()
		return nil, fmt.Errorf("loading permissions: %w", err)
	}
	views, err := store.LoadViews(info.Name)
	if err != nil {
		phase.SetStatus(codes.Error, "tenant configuration load failed")
		phase.End()
		return nil, fmt.Errorf("loading views: %w", err)
	}
	minKoraVersion, err := ActiveConfigMinKoraVersionContext(ctx, database, dialect, info.Name)
	if err != nil {
		phase.SetStatus(codes.Error, "tenant configuration version load failed")
		phase.End()
		return nil, fmt.Errorf("loading active config version: %w", err)
	}
	phase.End()
	if minKoraVersion != "" && binaryVersion != "" && !doctype.MinVersionOK(binaryVersion, minKoraVersion) {
		slog.Warn("active config version requires newer kora binary", "site", info.Name, "required", minKoraVersion, "running", binaryVersion)
	}
	registry := doctype.NewRegistry()
	registry.LoadFull(doctypes, roles, permissions)
	registry.Views.LoadFromDB(views)
	_, phase = tracer.Start(ctx, "engine.tenant.migrate_schema")
	err = schema.MigrateSiteFromRegistry(database, siteCfg.DBName, registry, dialect)
	if err != nil {
		phase.SetStatus(codes.Error, "tenant schema migration failed")
	}
	phase.End()
	if err != nil {
		return nil, fmt.Errorf("migrating site schema: %w", err)
	}
	if sessionManager == nil {
		sessionManager = auth.NewOwnedSessionManagerPaused(database).SetDialect(dialect)
	}
	runtime = &knet.LoadedSite{
		SiteID: info.SiteID, ConfigRevision: info.ConfigRevision, DatabaseIdentity: databaseIdentity, Status: info.Status, DBType: siteCfg.DBType,
		Name: info.Name,
		Config: knet.SiteRouterConfig{
			Hostname:      info.Name,
			Domains:       siteCfg.Domains(),
			FileStorage:   siteCfg.FileStorage,
			StorageBucket: siteCfg.StorageBucket,
		},
		DB: database, Registry: registry, SessionManager: sessionManager,
	}
	transferred = true
	return runtime, nil
}

// ActiveConfigMinKoraVersion reads the binary-version floor pinned by the
// active tenant config snapshot. An empty result means no active version exists.
func ActiveConfigMinKoraVersion(database *sql.DB, dialect kdb.Dialect, siteName string) (string, error) {
	return ActiveConfigMinKoraVersionContext(context.Background(), database, dialect, siteName)
}

// ActiveConfigMinKoraVersionContext reads the pinned binary-version floor
// using the tenant activation context so a canceled reload stops promptly.
func ActiveConfigMinKoraVersionContext(ctx context.Context, database *sql.DB, dialect kdb.Dialect, siteName string) (string, error) {
	if ctx == nil {
		return "", errors.New("active config version lookup requires context")
	}
	var minVersion string
	err := database.QueryRowContext(ctx, kdb.Rebind(dialect,
		"SELECT COALESCE(min_kora_version, '') FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1"),
		siteName,
	).Scan(&minVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return minVersion, nil
}

func canReuseTenantResources(existing *knet.LoadedSite, databaseIdentity string) bool {
	return existing != nil && existing.DB != nil && existing.SessionManager != nil && databaseIdentity != "" && existing.DatabaseIdentity == databaseIdentity
}
