package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/sdk/management"
	"github.com/spf13/cobra"
)

type adminOptions struct {
	Profile   string
	URL       string
	TokenEnv  string
	AllowHTTP bool
	Timeout   time.Duration
	Config    string
}

func init() { rootCmd.AddCommand(newAdminCommand()) }

func newAdminCommand() *cobra.Command {
	opts := &adminOptions{Timeout: 30 * time.Second}
	root := &cobra.Command{Use: "admin", Short: "Manage tenant module lifecycle through the Engine management API"}
	root.PersistentFlags().StringVar(&opts.Profile, "profile", "", "Management profile (separate from remote business profiles)")
	root.PersistentFlags().StringVar(&opts.URL, "url", "", "Engine management base URL")
	root.PersistentFlags().StringVar(&opts.TokenEnv, "token-env", "", "Environment variable containing the management bearer token")
	root.PersistentFlags().BoolVar(&opts.AllowHTTP, "allow-http", false, "Allow unencrypted HTTP for a non-local host")
	root.PersistentFlags().DurationVar(&opts.Timeout, "timeout", 30*time.Second, "Request timeout")
	root.PersistentFlags().StringVar(&opts.Config, "config", "", "Management profile file (defaults to the user config directory)")
	root.AddCommand(newAdminProfileCommand(opts), newAdminModulesCommand(opts), newAdminOperationCommand(opts), newAdminExecutionCommand(opts))
	return root
}

func newAdminProfileCommand(opts *adminOptions) *cobra.Command {
	root := &cobra.Command{Use: "profile", Short: "Manage management-only profiles"}
	var urlValue, tokenEnv string
	var allowHTTP bool
	set := &cobra.Command{Use: "set <name>", Short: "Save a management API profile", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(urlValue) == "" {
			return errors.New("--url is required")
		}
		if tokenEnv == "" {
			tokenEnv = "KORA_MANAGEMENT_TOKEN"
		}
		if _, err := validatedRemoteBaseURL(urlValue, allowHTTP); err != nil {
			return err
		}
		file, err := adminConfigPath(opts.Config)
		if err != nil {
			return err
		}
		cfg, err := loadRemoteProfiles(file)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(args[0])
		if name == "" {
			return errors.New("management profile name is required")
		}
		cfg.Profiles[name] = remoteProfile{URL: strings.TrimRight(urlValue, "/"), TokenEnv: tokenEnv, AllowHTTP: allowHTTP}
		if cfg.Current == "" {
			cfg.Current = name
		}
		if err := saveRemoteProfiles(file, cfg); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved management profile %q (token from %s).\n", name, tokenEnv)
		return err
	}}
	set.Flags().StringVar(&urlValue, "url", "", "Engine management URL")
	set.Flags().StringVar(&tokenEnv, "token-env", "KORA_MANAGEMENT_TOKEN", "Environment variable containing the management bearer token")
	set.Flags().BoolVar(&allowHTTP, "allow-http", false, "Allow HTTP for a non-local host")
	list := &cobra.Command{Use: "list", Short: "List management profiles", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		file, err := adminConfigPath(opts.Config)
		if err != nil {
			return err
		}
		cfg, err := loadRemoteProfiles(file)
		if err != nil {
			return err
		}
		for name, profile := range cfg.Profiles {
			marker := " "
			if name == cfg.Current {
				marker = "*"
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s\t%s\ttoken=%s\n", marker, name, profile.URL, profile.TokenEnv); err != nil {
				return err
			}
		}
		return nil
	}}
	use := &cobra.Command{Use: "use <name>", Args: cobra.ExactArgs(1), Short: "Select the default management profile", RunE: func(cmd *cobra.Command, args []string) error {
		file, err := adminConfigPath(opts.Config)
		if err != nil {
			return err
		}
		cfg, err := loadRemoteProfiles(file)
		if err != nil {
			return err
		}
		if _, ok := cfg.Profiles[args[0]]; !ok {
			return fmt.Errorf("management profile %q does not exist", args[0])
		}
		cfg.Current = args[0]
		return saveRemoteProfiles(file, cfg)
	}}
	root.AddCommand(set, list, use)
	return root
}

func newAdminModulesCommand(opts *adminOptions) *cobra.Command {
	root := &cobra.Command{Use: "modules", Short: "Inspect and govern tenant modules"}
	inspect := &cobra.Command{Use: "inspect <tenant-id>", Args: cobra.ExactArgs(1), Short: "Read redacted module inspection", RunE: func(cmd *cobra.Command, args []string) error {
		tenant, err := contract.ParseTenantID(args[0])
		if err != nil {
			return err
		}
		client, err := opts.client()
		if err != nil {
			return err
		}
		result, err := client.InspectModules(cmd.Context(), tenant)
		if err != nil {
			return classifyManagementError(err, opts.token())
		}
		return writeRemoteJSON(cmd.OutOrStdout(), result)
	}}
	var targetVersion, providerSelections string
	var expectedRevision uint64
	var targetState string
	plan := &cobra.Command{Use: "plan <tenant-id> <module-id>", Args: cobra.ExactArgs(2), Short: "Plan a module lifecycle change", RunE: func(cmd *cobra.Command, args []string) error {
		tenant, err := contract.ParseTenantID(args[0])
		if err != nil {
			return err
		}
		selections := []contract.ProviderSelection{}
		if providerSelections != "" {
			if err := decodeStrictJSON([]byte(providerSelections), &selections); err != nil {
				return fmt.Errorf("decode --provider-selections: %w", err)
			}
		}
		request := contract.PlanModuleChange{ModuleID: contract.ModuleID(args[1]), TargetState: contract.ModuleLifecycleState(targetState), TargetVersion: targetVersion, ProviderSelections: selections, ExpectedRevision: expectedRevision}
		client, err := opts.client()
		if err != nil {
			return err
		}
		result, err := client.PlanModuleChange(cmd.Context(), tenant, request)
		if err != nil {
			return classifyManagementError(err, opts.token())
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), result); err != nil {
			return err
		}
		if len(result.DependencyBlockers)+len(result.OpenWorkBlockers) > 0 {
			return cliOutcomeError(ExitConflict, errors.New("module lifecycle plan is blocked; review the returned blockers"))
		}
		return nil
	}}
	plan.Flags().StringVar(&targetState, "target-state", "", "Target module state: active, draining, disabled, or removed")
	plan.Flags().StringVar(&targetVersion, "target-version", "", "Requested module version")
	plan.Flags().StringVar(&providerSelections, "provider-selections", "", "JSON array of explicit provider selections")
	plan.Flags().Uint64Var(&expectedRevision, "expected-revision", 0, "Canonical current module configuration revision")
	_ = plan.MarkFlagRequired("target-state")
	_ = plan.MarkFlagRequired("expected-revision")

	var planFile, idempotencyKey string
	var wait bool
	var pollInterval time.Duration
	apply := &cobra.Command{Use: "apply <tenant-id>", Args: cobra.ExactArgs(1), Short: "Apply the exact plan returned by Engine", RunE: func(cmd *cobra.Command, args []string) error {
		if planFile == "" || idempotencyKey == "" {
			return errors.New("--plan-file and --idempotency-key are required")
		}
		data, err := readBoundedFile(planFile, 2<<20)
		if err != nil {
			return err
		}
		var plan contract.ModuleChangePlan
		if err := decodeStrictJSON(data, &plan); err != nil {
			return fmt.Errorf("decode module plan: %w", err)
		}
		if len(plan.DependencyBlockers)+len(plan.OpenWorkBlockers) > 0 {
			_ = writeRemoteJSON(cmd.OutOrStdout(), plan)
			return cliOutcomeError(ExitConflict, errors.New("cannot apply a blocked module plan"))
		}
		tenant, err := contract.ParseTenantID(args[0])
		if err != nil {
			return err
		}
		command := contract.ApplyModuleChange{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(idempotencyKey)}, ModuleID: plan.ModuleID, TargetState: plan.TargetState, TargetVersion: plan.TargetVersion, ProviderSelections: plan.ProviderSelections, PlanHash: plan.PlanHash, ExpectedRevision: plan.BaseRevision}
		client, err := opts.client()
		if err != nil {
			return err
		}
		operation, err := client.ApplyModuleChange(cmd.Context(), tenant, command)
		if err != nil {
			return classifyManagementError(err, opts.token())
		}
		if wait {
			operation = waitForManagementOperation(cmd.Context(), client, operation, pollInterval)
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), operation); err != nil {
			return err
		}
		return managementOperationExit(operation)
	}}
	apply.Flags().StringVar(&planFile, "plan-file", "", "File containing the exact Engine ModuleChangePlan JSON")
	apply.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Stable key for this lifecycle apply intent")
	apply.Flags().BoolVar(&wait, "wait", false, "Poll the accepted operation without resubmitting apply")
	apply.Flags().DurationVar(&pollInterval, "poll-interval", time.Second, "Management operation polling interval")
	root.AddCommand(inspect, plan, apply)
	return root
}

func newAdminOperationCommand(opts *adminOptions) *cobra.Command {
	root := &cobra.Command{Use: "operation", Short: "Poll durable management operations"}
	get := &cobra.Command{Use: "inspect <operation-id>", Args: cobra.ExactArgs(1), Short: "Read a submitted management operation", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := contract.ParseOperationID(args[0])
		if err != nil {
			return err
		}
		client, err := opts.client()
		if err != nil {
			return err
		}
		operation, err := client.Operation(cmd.Context(), id)
		if err != nil {
			return classifyManagementError(err, opts.token())
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), operation); err != nil {
			return err
		}
		return managementOperationExit(operation)
	}}
	root.AddCommand(get)
	return root
}

func newAdminExecutionCommand(opts *adminOptions) *cobra.Command {
	root := &cobra.Command{Use: "execution", Short: "Inspect pinned business executions with management authority"}
	inspect := &cobra.Command{Use: "inspect <tenant-id> <execution-id>", Args: cobra.ExactArgs(2), Short: "Read redacted execution diagnostics", RunE: func(cmd *cobra.Command, args []string) error {
		tenant, err := contract.ParseTenantID(args[0])
		if err != nil {
			return err
		}
		client, err := opts.client()
		if err != nil {
			return err
		}
		inspection, err := client.InspectExecution(cmd.Context(), tenant, args[1])
		if err != nil {
			return classifyManagementError(err, opts.token())
		}
		return writeRemoteJSON(cmd.OutOrStdout(), inspection)
	}}
	root.AddCommand(inspect)
	return root
}

func (o *adminOptions) client() (management.Client, error) {
	file, err := adminConfigPath(o.Config)
	if err != nil {
		return nil, err
	}
	cfg, err := loadRemoteProfiles(file)
	if err != nil {
		return nil, err
	}
	name := o.Profile
	if name == "" {
		name = cfg.Current
	}
	profile, ok := cfg.Profiles[name]
	if !ok {
		return nil, errors.New("configure a separate management profile with `kora admin profile set`")
	}
	if o.URL != "" {
		profile.URL = o.URL
	}
	if o.TokenEnv != "" {
		profile.TokenEnv = o.TokenEnv
	}
	if o.AllowHTTP {
		profile.AllowHTTP = true
	}
	if profile.TokenEnv == "" {
		profile.TokenEnv = "KORA_MANAGEMENT_TOKEN"
	}
	token := strings.TrimSpace(os.Getenv(profile.TokenEnv))
	if token == "" {
		return nil, fmt.Errorf("%s is not set", profile.TokenEnv)
	}
	base, err := validatedRemoteBaseURL(profile.URL, profile.AllowHTTP)
	if err != nil {
		return nil, err
	}
	return management.NewClient(management.Config{BaseURL: base.String(), HTTPClient: &http.Client{Timeout: o.timeout()}, Tokens: management.TokenSourceFunc(func(context.Context) (string, error) { return token, nil })})
}

func (o *adminOptions) token() string {
	file, err := adminConfigPath(o.Config)
	if err != nil {
		return ""
	}
	cfg, err := loadRemoteProfiles(file)
	if err != nil {
		return ""
	}
	name := o.Profile
	if name == "" {
		name = cfg.Current
	}
	profile := cfg.Profiles[name]
	if o.TokenEnv != "" {
		profile.TokenEnv = o.TokenEnv
	}
	if profile.TokenEnv == "" {
		profile.TokenEnv = "KORA_MANAGEMENT_TOKEN"
	}
	return strings.TrimSpace(os.Getenv(profile.TokenEnv))
}

func (o *adminOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 30 * time.Second
	}
	return o.Timeout
}

func adminConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "kora", "admin.json"), nil
}

func readBoundedFile(filename string, limit int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // read-only file
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", filename, limit)
	}
	return data, nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("JSON input must contain exactly one value")
	}
	return nil
}

func waitForManagementOperation(ctx context.Context, client management.Client, operation contract.ManagedOperation, interval time.Duration) contract.ManagedOperation {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for operation.State == contract.OperationRequested || operation.State == contract.OperationRunning {
		select {
		case <-ctx.Done():
			return operation
		case <-ticker.C:
			current, err := client.Operation(ctx, operation.ID)
			if err != nil {
				return operation
			}
			operation = current
		}
	}
	return operation
}

func managementOperationExit(operation contract.ManagedOperation) error {
	switch operation.State {
	case contract.OperationSucceeded:
		return nil
	case contract.OperationRequested, contract.OperationRunning:
		return cliOutcomeError(ExitAccepted, fmt.Errorf("management operation remains %s", operation.State))
	case contract.OperationFailed:
		return cliOutcomeError(ExitRejected, errors.New("management operation failed"))
	case contract.OperationRecoveryRequired:
		return cliOutcomeError(ExitUnknown, errors.New("management operation requires recovery; inspect and reconcile the existing operation"))
	default:
		return cliOutcomeError(ExitUnavailable, errors.New("management operation has an unsupported state"))
	}
}

func classifyManagementError(err error, token string) error {
	var remote *management.RemoteError
	if errors.As(err, &remote) {
		message := safeRemoteMessage(err, token)
		switch {
		case remote.Status == http.StatusConflict:
			return cliOutcomeError(ExitConflict, errors.New(message))
		case remote.Status == http.StatusServiceUnavailable || remote.Status == http.StatusTooManyRequests:
			return cliOutcomeError(ExitUnavailable, errors.New(message))
		case remote.Status == http.StatusForbidden || remote.Status == http.StatusUnauthorized || remote.Status == http.StatusBadRequest:
			return cliOutcomeError(ExitRejected, errors.New(message))
		}
	}
	return cliOutcomeError(ExitUnavailable, errors.New(safeRemoteMessage(err, token)))
}
