package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/script"
)

type sharedRuntimeTestRunner struct{ closed int }

func (*sharedRuntimeTestRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	return nil, nil
}
func (*sharedRuntimeTestRunner) Validate(string) error { return nil }
func (r *sharedRuntimeTestRunner) Close() error {
	r.closed++
	return nil
}

func TestInitializeSharedRuntimeRegistersScriptRunnerBeforeOptionalNATSFailure(t *testing.T) {
	bootstrapErr := errors.New("stream bootstrap failed")
	runner := &sharedRuntimeTestRunner{}
	provider := &natsprovider.Provider{}
	closed := 0
	var gotConfig natsprovider.Config
	resources := &ProcessResources{}
	result, err := initializeSharedRuntime(context.Background(), SharedRuntimeConfig{
		ScriptsEnabled: true, NATSConfig: &natsprovider.Config{ServerURLs: []string{"nats://broker.invalid:4222"}},
		BootstrapSiteDirectory: true,
	}, resources, sharedRuntimeDependencies{
		newScriptRunner: func() script.Runner { return runner },
		newNATS: func(_ context.Context, config natsprovider.Config) (*natsprovider.Provider, error) {
			gotConfig = config
			return provider, nil
		},
		bootstrapNATS: func(_ context.Context, _ natsBootstrapper, bootstrapDirectory bool) error {
			if !bootstrapDirectory {
				t.Fatal("site-directory bootstrap flag was not forwarded")
			}
			return bootstrapErr
		},
		closeNATS: func(got natsBootstrapper) {
			if got != provider {
				t.Fatal("closed an unexpected NATS provider")
			}
			closed++
		},
	})
	if err != nil {
		t.Fatalf("initializeSharedRuntime: %v", err)
	}
	if result == nil || result.ScriptRunner != runner || result.NATSProvider != nil {
		t.Fatalf("shared runtime result = %#v", result)
	}
	if resources.ScriptRunner != runner || resources.NATSProvider != nil {
		t.Fatalf("registered process resources = script %v NATS %v", resources.ScriptRunner, resources.NATSProvider)
	}
	if closed != 1 {
		t.Fatalf("NATS close count = %d, want one", closed)
	}
	if gotConfig.Name != "kora-engine" {
		t.Fatalf("NATS provider name = %q, want kora-engine", gotConfig.Name)
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown acquired script runner: %v", err)
	}
	if runner.closed != 1 {
		t.Fatalf("script runner close count = %d, want one", runner.closed)
	}
}

func TestInitializeSharedRuntimeWithoutNATSKeepsSelfHostedMode(t *testing.T) {
	resources := &ProcessResources{}
	runner := &sharedRuntimeTestRunner{}
	result, err := initializeSharedRuntime(context.Background(), SharedRuntimeConfig{ScriptsEnabled: true}, resources, sharedRuntimeDependencies{
		newScriptRunner: func() script.Runner { return runner },
	})
	if err != nil {
		t.Fatalf("initializeSharedRuntime: %v", err)
	}
	if result.ScriptRunner != runner || result.NATSProvider != nil || resources.NATSProvider != nil {
		t.Fatalf("local shared runtime = %#v, resources NATS=%v", result, resources.NATSProvider)
	}
}
