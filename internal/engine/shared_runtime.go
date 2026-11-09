package engine

import (
	"context"
	"errors"
	"log/slog"

	"github.com/asenawritescode/kora/internal/natsprovider"
	"github.com/asenawritescode/kora/internal/script"
)

// SharedRuntimeConfig contains process-wide execution providers. NATS is
// optional: connection or bootstrap failure preserves the supported local
// SQL-polling and in-process delivery mode.
type SharedRuntimeConfig struct {
	ScriptsEnabled         bool
	NATSConfig             *natsprovider.Config
	BootstrapSiteDirectory bool
}

// SharedRuntime is the typed provider set consumed by tenant service assembly.
type SharedRuntime struct {
	ScriptRunner script.Runner
	NATSProvider *natsprovider.Provider
}

type natsBootstrapper interface {
	Bootstrap(context.Context) error
	BootstrapSiteDirectory(context.Context) error
	Close()
}

type sharedRuntimeDependencies struct {
	newScriptRunner func() script.Runner
	newNATS         func(context.Context, natsprovider.Config) (*natsprovider.Provider, error)
	bootstrapNATS   func(context.Context, natsBootstrapper, bool) error
	closeNATS       func(natsBootstrapper)
}

// InitializeSharedRuntime constructs process-wide script and optional NATS
// providers and registers each acquired resource with ProcessResources before
// it can be used by tenant workers.
func InitializeSharedRuntime(ctx context.Context, config SharedRuntimeConfig, resources *ProcessResources) (*SharedRuntime, error) {
	return initializeSharedRuntime(ctx, config, resources, sharedRuntimeDependencies{
		newScriptRunner: func() script.Runner { return script.NewEmbeddedRunner(script.DefaultEmbeddedConfig()) },
		newNATS:         natsprovider.New,
		bootstrapNATS: func(ctx context.Context, provider natsBootstrapper, bootstrapDirectory bool) error {
			if err := provider.Bootstrap(ctx); err != nil {
				return err
			}
			if bootstrapDirectory {
				return provider.BootstrapSiteDirectory(ctx)
			}
			return nil
		},
		closeNATS: func(provider natsBootstrapper) { provider.Close() },
	})
}

func initializeSharedRuntime(ctx context.Context, config SharedRuntimeConfig, resources *ProcessResources, deps sharedRuntimeDependencies) (*SharedRuntime, error) {
	if ctx == nil || resources == nil {
		return nil, errors.New("shared runtime requires context and process resource owner")
	}
	if config.ScriptsEnabled && deps.newScriptRunner == nil {
		return nil, errors.New("shared runtime script runner constructor is required")
	}
	if config.NATSConfig != nil && (deps.newNATS == nil || deps.bootstrapNATS == nil || deps.closeNATS == nil) {
		return nil, errors.New("shared runtime NATS constructors and lifecycle are required")
	}
	result := &SharedRuntime{}
	if config.ScriptsEnabled {
		result.ScriptRunner = deps.newScriptRunner()
		if result.ScriptRunner == nil {
			return nil, errors.New("shared runtime script runner constructor returned nil")
		}
		resources.ScriptRunner = result.ScriptRunner
		slog.Info("script runner initialized", "pool_size", script.DefaultEmbeddedConfig().PoolSize)
	} else {
		slog.Info("script runner disabled")
	}
	if config.NATSConfig == nil {
		return result, nil
	}
	natsConfig := *config.NATSConfig
	natsConfig.Name = "kora-engine"
	provider, err := deps.newNATS(ctx, natsConfig)
	if err != nil {
		slog.Warn("NATS unavailable; continuing with SQL directory polling and local event delivery", "error", err)
		return result, nil
	}
	if provider == nil {
		slog.Warn("NATS unavailable; continuing with SQL directory polling and local event delivery", "error", "provider constructor returned nil")
		return result, nil
	}
	if err := deps.bootstrapNATS(ctx, provider, config.BootstrapSiteDirectory); err != nil {
		deps.closeNATS(provider)
		slog.Warn("NATS bootstrap failed; continuing with SQL directory polling and local event delivery", "error", err)
		return result, nil
	}
	result.NATSProvider = provider
	resources.NATSProvider = provider
	return result, nil
}
