package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	tenantsdk "github.com/asenawritescode/kora/sdk/tenant"
	"github.com/spf13/cobra"
)

const (
	ExitAccepted    = 10
	ExitRejected    = 20
	ExitConflict    = 21
	ExitUnavailable = 30
	ExitUnknown     = 40
)

// CLIExitError carries a stable machine-readable process exit status.
type CLIExitError struct {
	Code int
	Err  error
}

func (e *CLIExitError) Error() string { return e.Err.Error() }
func (e *CLIExitError) Unwrap() error { return e.Err }

func newRemoteCatalogCommand(opts *remoteOptions) *cobra.Command {
	return &cobra.Command{Use: "catalog", Short: "Read the actor-filtered published module catalog", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		catalog, err := client.Catalog(cmd.Context())
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		return writeRemoteJSON(cmd.OutOrStdout(), catalog)
	}}
}

func newRemoteQueryCommand(opts *remoteOptions) *cobra.Command {
	var input, file, cursor string
	var limit uint16
	cmd := &cobra.Command{
		Use: "query <owner-module> <query-name>", Short: "Run an actor-authorized published query", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, err := readAnyRemotePayload(cmd.InOrStdin(), input, file)
			if err != nil {
				return err
			}
			client, err := opts.businessClient()
			if err != nil {
				return err
			}
			catalog, err := client.Catalog(cmd.Context())
			if err != nil {
				return classifyTenantError(err, clientToken(opts))
			}
			result, err := client.PublishedQuery(cmd.Context(), catalog, contract.ModuleID(args[0]), args[1], contract.PublishedQueryRequest{Input: payload, Cursor: cursor, Limit: limit})
			if err != nil {
				return classifyTenantError(err, clientToken(opts))
			}
			return writeRemoteJSON(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "JSON query input")
	cmd.Flags().StringVarP(&file, "file", "f", "", "Read JSON query input from a file, or - for stdin")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Opaque cursor returned by the previous page")
	cmd.Flags().Uint16Var(&limit, "limit", 0, "Page size (zero uses the published default)")
	return cmd
}

func newRemoteOperationCommand(opts *remoteOptions) *cobra.Command {
	root := &cobra.Command{Use: "operation", Short: "Execute and inspect published domain operations"}
	var input, file, idempotencyKey, expectedRevision string
	var wait bool
	var pollInterval time.Duration
	execute := &cobra.Command{
		Use: "execute <operation-ref>", Short: "Execute one published operation with a stable intent key", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if idempotencyKey == "" {
				return errors.New("--idempotency-key is required; reuse it for every retry of this intent")
			}
			payload, err := readAnyRemotePayload(cmd.InOrStdin(), input, file)
			if err != nil {
				return err
			}
			client, err := opts.businessClient()
			if err != nil {
				return err
			}
			catalog, err := client.Catalog(cmd.Context())
			if err != nil {
				return classifyTenantError(err, clientToken(opts))
			}
			operationRef := args[0]
			var published *contract.ApplicationCatalogOperation
			for i := range catalog.PublishedOperations {
				if catalog.PublishedOperations[i].OperationRef == operationRef {
					published = &catalog.PublishedOperations[i]
					break
				}
			}
			if published == nil {
				return errors.New("operation is not present in the actor-filtered catalog")
			}
			command := contract.ExecutePublishedOperation{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(idempotencyKey), ExpectedRevision: contract.Revision(expectedRevision)}, InputSchema: published.InputSchema, Input: payload}
			receipt, executeErr := client.ExecutePublishedOperation(cmd.Context(), catalog, operationRef, command)
			if executeErr != nil {
				var remote *tenantsdk.RemoteError
				if !errors.As(executeErr, &remote) || remote.Status >= 500 {
					lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 2*time.Second)
					defer cancel()
					recovered, lookupErr := client.LookupExecution(lookupCtx, catalog, contract.LookupPublishedOperation{OperationRef: operationRef, IdempotencyKey: contract.IdempotencyKey(idempotencyKey)})
					if lookupErr == nil {
						receipt = recovered
					} else if errors.As(lookupErr, &remote) && remote.Status == http.StatusNotFound {
						return cliOutcomeError(ExitUnknown, fmt.Errorf("execution response was lost and no receipt is currently visible; retry lookup with the same operation and idempotency key"))
					} else {
						return cliOutcomeError(ExitUnknown, fmt.Errorf("execution outcome is unknown; recover with the same operation and idempotency key: %s", safeRemoteMessage(lookupErr, clientToken(opts))))
					}
				} else {
					return classifyTenantError(executeErr, clientToken(opts))
				}
			}
			if wait {
				receipt = waitForOperation(cmd.Context(), client, catalog, receipt, pollInterval)
			}
			if err := writeRemoteJSON(cmd.OutOrStdout(), receipt); err != nil {
				return err
			}
			return receiptExit(receipt)
		},
	}
	execute.Flags().StringVar(&input, "input", "", "JSON operation input")
	execute.Flags().StringVarP(&file, "file", "f", "", "Read JSON operation input from a file, or - for stdin")
	execute.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Stable identity for this business intent")
	execute.Flags().StringVar(&expectedRevision, "expected-revision", "", "Optional canonical revision precondition")
	execute.Flags().BoolVar(&wait, "wait", false, "Poll the accepted execution without submitting it again")
	execute.Flags().DurationVar(&pollInterval, "poll-interval", time.Second, "Receipt polling interval when --wait is set")

	inspect := &cobra.Command{
		Use: "inspect <execution-id>", Short: "Read an authorized operation receipt by execution ID", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := opts.businessClient()
			if err != nil {
				return err
			}
			catalog, err := client.Catalog(cmd.Context())
			if err != nil {
				return classifyTenantError(err, clientToken(opts))
			}
			id, err := contract.ParseOperationID(args[0])
			if err != nil {
				return err
			}
			receipt, err := client.Execution(cmd.Context(), catalog, id)
			if err != nil {
				return classifyTenantError(err, clientToken(opts))
			}
			if err := writeRemoteJSON(cmd.OutOrStdout(), receipt); err != nil {
				return err
			}
			return receiptExit(receipt)
		},
	}
	lookup := &cobra.Command{
		Use: "lookup <operation-ref>", Short: "Recover a receipt using the original idempotency key without dispatching", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if idempotencyKey == "" {
				return errors.New("--idempotency-key is required")
			}
			client, err := opts.businessClient()
			if err != nil {
				return err
			}
			catalog, err := client.Catalog(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := client.LookupExecution(cmd.Context(), catalog, contract.LookupPublishedOperation{OperationRef: args[0], IdempotencyKey: contract.IdempotencyKey(idempotencyKey)})
			if err != nil {
				return classifyTenantError(err, clientToken(opts))
			}
			if err := writeRemoteJSON(cmd.OutOrStdout(), receipt); err != nil {
				return err
			}
			return receiptExit(receipt)
		},
	}
	lookup.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Original stable intent key")
	root.AddCommand(execute, inspect, lookup)
	return root
}

func newRemoteWorkflowCommand(opts *remoteOptions) *cobra.Command {
	root := &cobra.Command{Use: "workflow", Short: "Start, inspect and reconcile durable workflows"}
	var definitionID, subjectJSON, startKey string
	var cancelKey, cancelReason, stepID, outcome, evidenceRef string
	var reconcileKey, reconcileReason string
	var definitionVersion uint32
	start := &cobra.Command{Use: "start", Args: cobra.NoArgs, Short: "Start a workflow with a stable intent key", RunE: func(cmd *cobra.Command, _ []string) error {
		if startKey == "" {
			return errors.New("--idempotency-key is required")
		}
		var subject contract.SubjectRef
		if err := decodeStrictJSON([]byte(subjectJSON), &subject); err != nil {
			return fmt.Errorf("decode --subject: %w", err)
		}
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		instance, err := client.StartWorkflow(cmd.Context(), contract.StartWorkflow{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(startKey)}, DefinitionID: contract.DefinitionID(definitionID), DefinitionVersion: definitionVersion, Subject: subject})
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), instance); err != nil {
			return err
		}
		return workflowExit(instance)
	}}
	start.Flags().StringVar(&definitionID, "definition", "", "Published workflow definition ID")
	start.Flags().Uint32Var(&definitionVersion, "version", 0, "Exact published workflow version")
	start.Flags().StringVar(&subjectJSON, "subject", "", "Versioned subject JSON, for example {\"kind\":\"sales.order\",\"id\":\"ORD-1\",\"revision\":\"4\"}")
	start.Flags().StringVar(&startKey, "idempotency-key", "", "Stable workflow start intent key")
	_ = start.MarkFlagRequired("definition")
	_ = start.MarkFlagRequired("version")
	_ = start.MarkFlagRequired("subject")
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List recent actor-visible workflows", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		instances, err := client.WorkflowInstances(cmd.Context())
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		return writeRemoteJSON(cmd.OutOrStdout(), instances)
	}}
	inspect := &cobra.Command{Use: "inspect <instance-id>", Args: cobra.ExactArgs(1), Short: "Inspect a workflow instance", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := contract.ParseInstanceID(args[0])
		if err != nil {
			return err
		}
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		instance, err := client.Workflow(cmd.Context(), id)
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), instance); err != nil {
			return err
		}
		return workflowExit(instance)
	}}
	history := &cobra.Command{Use: "history <instance-id>", Args: cobra.ExactArgs(1), Short: "Read payload-free workflow history", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := contract.ParseInstanceID(args[0])
		if err != nil {
			return err
		}
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		entries, err := client.WorkflowHistory(cmd.Context(), id)
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		return writeRemoteJSON(cmd.OutOrStdout(), entries)
	}}
	cancel := &cobra.Command{Use: "cancel <instance-id>", Args: cobra.ExactArgs(1), Short: "Cancel an existing workflow with a stable command key", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := contract.ParseInstanceID(args[0])
		if err != nil {
			return err
		}
		if cancelKey == "" {
			return errors.New("--idempotency-key is required")
		}
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		instance, err := client.CancelWorkflow(cmd.Context(), id, contract.CancelWorkflow{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(cancelKey)}, Reason: cancelReason})
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), instance); err != nil {
			return err
		}
		return workflowExit(instance)
	}}
	reconcile := &cobra.Command{Use: "reconcile <instance-id>", Args: cobra.ExactArgs(1), Short: "Record an evidence-based unknown workflow outcome", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := contract.ParseInstanceID(args[0])
		if err != nil {
			return err
		}
		if reconcileKey == "" {
			return errors.New("--idempotency-key is required")
		}
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		instance, err := client.ReconcileWorkflow(cmd.Context(), id, contract.ReconcileWorkflow{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(reconcileKey)}, StepID: contract.StepID(stepID), Outcome: contract.ReconciliationOutcome(outcome), EvidenceRef: evidenceRef, Reason: reconcileReason})
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), instance); err != nil {
			return err
		}
		return workflowExit(instance)
	}}
	cancel.Flags().StringVar(&cancelKey, "idempotency-key", "", "Stable cancel intent key")
	cancel.Flags().StringVar(&cancelReason, "reason", "", "Reason for cancellation")
	_ = cancel.MarkFlagRequired("idempotency-key")
	_ = cancel.MarkFlagRequired("reason")
	reconcile.Flags().StringVar(&reconcileKey, "idempotency-key", "", "Stable reconciliation intent key")
	reconcile.Flags().StringVar(&stepID, "step-id", "", "Uncertain workflow step ID")
	reconcile.Flags().StringVar(&outcome, "outcome", "", "confirmed_succeeded, confirmed_not_applied, or abandon")
	reconcile.Flags().StringVar(&evidenceRef, "evidence-ref", "", "Reference to operator evidence")
	reconcile.Flags().StringVar(&reconcileReason, "reason", "", "Reconciliation reason")
	_ = reconcile.MarkFlagRequired("idempotency-key")
	_ = reconcile.MarkFlagRequired("step-id")
	_ = reconcile.MarkFlagRequired("outcome")
	_ = reconcile.MarkFlagRequired("reason")
	root.AddCommand(start, list, inspect, history, cancel, reconcile)
	return root
}

func newRemoteReconciliationCommand(opts *remoteOptions) *cobra.Command {
	root := &cobra.Command{Use: "reconciliation", Aliases: []string{"reconcile"}, Short: "Inspect and act on owner-governed business reconciliation findings"}
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "Count unresolved findings for the selected scope", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		result, err := client.BusinessReconciliationSummary(cmd.Context())
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		return writeRemoteJSON(cmd.OutOrStdout(), result)
	}}
	var owner, ruleID, runID, previousRunID string
	run := &cobra.Command{Use: "run", Args: cobra.NoArgs, Short: "Run or continue an owner-published reconciliation rule", RunE: func(cmd *cobra.Command, _ []string) error {
		request := contract.BusinessReconciliationRunRequest{OwnerModuleID: contract.ModuleID(owner), RuleID: ruleID, RunID: runID, PreviousRunID: previousRunID}
		if err := request.Validate(); err != nil {
			return err
		}
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		result, err := client.RunBusinessReconciliation(cmd.Context(), request)
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), result); err != nil {
			return err
		}
		if !result.Complete {
			return cliOutcomeError(ExitUnavailable, errors.New("reconciliation run is incomplete; inspect its pinned watermarks and continue the same run chain"))
		}
		return nil
	}}
	run.Flags().StringVar(&owner, "owner-module", "", "Owner module that published the reconciliation rule")
	run.Flags().StringVar(&ruleID, "rule", "", "Published reconciliation rule ID")
	run.Flags().StringVar(&runID, "run-id", "", "Stable new run ID; retries must reuse this ID")
	run.Flags().StringVar(&previousRunID, "previous-run-id", "", "Authorized incomplete run to continue")
	_ = run.MarkFlagRequired("owner-module")
	_ = run.MarkFlagRequired("rule")
	_ = run.MarkFlagRequired("run-id")
	inspect := &cobra.Command{Use: "inspect <owner-module> <rule-id> <run-id>", Args: cobra.ExactArgs(3), Short: "Read an authorized immutable reconciliation report", RunE: func(cmd *cobra.Command, args []string) error {
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		result, err := client.BusinessReconciliationRun(cmd.Context(), contract.ModuleID(args[0]), args[1], args[2])
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		if err := writeRemoteJSON(cmd.OutOrStdout(), result); err != nil {
			return err
		}
		if !result.Complete {
			return cliOutcomeError(ExitUnavailable, errors.New("reconciliation report is incomplete"))
		}
		return nil
	}}
	var findingID, reason string
	waive := &cobra.Command{Use: "waive <owner-module> <rule-id> <run-id>", Args: cobra.ExactArgs(3), Short: "Waive one authorized finding with an audit reason", RunE: func(cmd *cobra.Command, args []string) error {
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		result, err := client.WaiveBusinessFinding(cmd.Context(), contract.ModuleID(args[0]), args[1], args[2], contract.BusinessFindingDecision{FindingID: findingID, Reason: reason})
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		return writeRemoteJSON(cmd.OutOrStdout(), result)
	}}
	repair := &cobra.Command{Use: "repair <owner-module> <rule-id> <run-id>", Args: cobra.ExactArgs(3), Short: "Request repair through the owner-published operation", RunE: func(cmd *cobra.Command, args []string) error {
		client, err := opts.businessClient()
		if err != nil {
			return err
		}
		result, err := client.RepairBusinessFinding(cmd.Context(), contract.ModuleID(args[0]), args[1], args[2], contract.BusinessFindingDecision{FindingID: findingID})
		if err != nil {
			return classifyTenantError(err, clientToken(opts))
		}
		return writeRemoteJSON(cmd.OutOrStdout(), result)
	}}
	for _, command := range []*cobra.Command{waive, repair} {
		command.Flags().StringVar(&findingID, "finding-id", "", "Stable finding identifier from the inspected run")
		_ = command.MarkFlagRequired("finding-id")
	}
	waive.Flags().StringVar(&reason, "reason", "", "Reason recorded in the reconciliation audit")
	_ = waive.MarkFlagRequired("reason")
	root.AddCommand(list, run, inspect, waive, repair)
	return root
}

func workflowExit(instance contract.WorkflowInstance) error {
	switch instance.Status {
	case contract.InstanceCompleted, contract.InstanceCancelled:
		return nil
	case contract.InstanceReady, contract.InstanceRunning, contract.InstanceWaiting, contract.InstanceRetrying:
		return cliOutcomeError(ExitAccepted, fmt.Errorf("workflow remains %s", instance.Status))
	case contract.InstanceFailed:
		return cliOutcomeError(ExitRejected, errors.New("workflow failed"))
	case contract.InstanceUncertain:
		return cliOutcomeError(ExitUnknown, errors.New("workflow outcome is unknown; inspect and reconcile the existing instance"))
	default:
		return cliOutcomeError(ExitUnavailable, errors.New("workflow has an unsupported status"))
	}
}

func (o *remoteOptions) businessClient() (*tenantsdk.ScopedClient, error) {
	_, profile, base, err := o.remoteProfile()
	if err != nil {
		return nil, err
	}
	if profile.Site == "" {
		return nil, errors.New("remote profile must set --site for published business API routes")
	}
	if profile.TenantID == "" {
		return nil, errors.New("remote profile must set --tenant to the canonical Engine tenant ID")
	}
	var scope contract.BusinessScope
	if profile.LegalEntityID != "" {
		scope = contract.BusinessScope{TenantID: contract.TenantID(profile.TenantID), Kind: contract.ScopeKindLegalEntity, LegalEntityID: profile.LegalEntityID}
	} else {
		scope = contract.BusinessScope{TenantID: contract.TenantID(profile.TenantID), Kind: contract.ScopeKindTenant}
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("remote profile business scope: %w", err)
	}
	if profile.TokenEnv == "" {
		profile.TokenEnv = defaultRemoteTokenEnv
	}
	if profile.TokenEnv == "KORA_MANAGEMENT_TOKEN" {
		return nil, errors.New("management credentials cannot be configured for tenant business commands")
	}
	remote, _, err := o.client()
	if err != nil {
		return nil, err
	}
	token := remote.token
	if token == "" {
		return nil, fmt.Errorf("%s is not set and no valid saved session is available; published business commands require an actor or extension bearer credential", profile.TokenEnv)
	}
	client, err := tenantsdk.NewClient(tenantsdk.Config{BaseURL: base.String(), SiteName: profile.Site, HTTPClient: &http.Client{Timeout: o.timeout()}, Tokens: tenantsdk.TokenSourceFunc(func(context.Context) (string, error) { return token, nil })})
	if err != nil {
		return nil, err
	}
	return client.WithScope(scope)
}

func readAnyRemotePayload(stdin io.Reader, inline, file string) (json.RawMessage, error) {
	if inline != "" && file != "" {
		return nil, errors.New("use either --input or --file, not both")
	}
	var data []byte
	var err error
	switch {
	case inline != "":
		data = []byte(inline)
	case file == "-":
		data, err = io.ReadAll(io.LimitReader(stdin, (1<<20)+1))
	case file != "":
		data, err = os.ReadFile(file)
	default:
		return nil, errors.New("one of --input or --file is required")
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > 1<<20 || !json.Valid(data) {
		return nil, errors.New("input must be valid JSON no larger than 1 MiB")
	}
	return json.RawMessage(data), nil
}

func waitForOperation(ctx context.Context, client *tenantsdk.ScopedClient, catalog contract.ApplicationCatalog, receipt contract.OperationReceipt, interval time.Duration) contract.OperationReceipt {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for receipt.Status == contract.ReceiptAccepted || receipt.Status == contract.ReceiptRunning {
		select {
		case <-ctx.Done():
			return receipt
		case <-ticker.C:
			current, err := client.Execution(ctx, catalog, receipt.ExecutionID)
			if err != nil {
				return receipt
			}
			receipt = current
		}
	}
	return receipt
}

func receiptExit(receipt contract.OperationReceipt) error {
	switch receipt.Status {
	case contract.ReceiptCompleted:
		return nil
	case contract.ReceiptAccepted, contract.ReceiptRunning:
		return cliOutcomeError(ExitAccepted, fmt.Errorf("operation remains %s", receipt.Status))
	case contract.ReceiptRejected, contract.ReceiptFailed:
		return cliOutcomeError(ExitRejected, fmt.Errorf("operation ended with status %s", receipt.Status))
	case contract.ReceiptConflict:
		return cliOutcomeError(ExitConflict, errors.New("operation conflicted with current business state"))
	case contract.ReceiptOutcomeUnknown:
		return cliOutcomeError(ExitUnknown, errors.New("operation outcome is unknown; reconcile the existing receipt"))
	default:
		return cliOutcomeError(ExitUnavailable, errors.New("operation receipt has an unsupported status"))
	}
}

func cliOutcomeError(code int, err error) error { return &CLIExitError{Code: code, Err: err} }

func classifyTenantError(err error, token string) error {
	var remote *tenantsdk.RemoteError
	if errors.As(err, &remote) {
		switch {
		case remote.Status == http.StatusConflict:
			return cliOutcomeError(ExitConflict, errors.New(safeRemoteMessage(err, token)))
		case remote.Status == http.StatusServiceUnavailable || remote.Status == http.StatusTooManyRequests:
			return cliOutcomeError(ExitUnavailable, errors.New(safeRemoteMessage(err, token)))
		case remote.Status == http.StatusForbidden || remote.Status == http.StatusUnauthorized || remote.Status == http.StatusBadRequest:
			return cliOutcomeError(ExitRejected, errors.New(safeRemoteMessage(err, token)))
		case remote.Status == http.StatusNotFound:
			return cliOutcomeError(ExitRejected, errors.New(safeRemoteMessage(err, token)))
		}
	}
	return cliOutcomeError(ExitUnavailable, errors.New(safeRemoteMessage(err, token)))
}

func safeRemoteMessage(err error, token string) string {
	message := err.Error()
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	return message
}

func clientToken(opts *remoteOptions) string {
	_, profile, _, err := opts.remoteProfile()
	if err != nil {
		return ""
	}
	if profile.TokenEnv == "" {
		profile.TokenEnv = defaultRemoteTokenEnv
	}
	return strings.TrimSpace(os.Getenv(profile.TokenEnv))
}
