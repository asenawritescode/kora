package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api/ai"
	"github.com/asenawritescode/kora/internal/payment"
	"github.com/asenawritescode/kora/internal/policy"
)

// NewAIModuleOperations returns the tenant's published operations for one request as the AI
// pack-action tools see them.
func NewAIModuleOperations(h *Handler, c *gin.Context) ai.ModuleOperations {
	return aiModuleOperations{h: h, c: c}
}

// aiModuleOperations gives the AI pack-action tools the tenant's published operations for
// one request. Both methods reuse the catalog build and the operation run of the HTTP routes.
type aiModuleOperations struct {
	h *Handler
	c *gin.Context
}

func (m aiModuleOperations) Catalog(execution policy.Execution, scope contract.BusinessScope) (contract.ApplicationCatalog, error) {
	return m.h.buildApplicationCatalog(m.c, execution, scope)
}

// Run settles the payment lines that name no reference (a payment intent per line, keyed by the
// request's idempotency key), then runs the operation with the references filled in.
func (m aiModuleOperations) Run(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, ref string, request contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	settled, err := m.settlePaymentLines(ctx, execution, ref, request, true)
	if err != nil {
		return contract.OperationReceipt{}, err
	}
	return m.execute(ctx, publishedOperationRun{Execution: execution, Scope: scope, Ref: ref, Request: settled})
}

// pendingPaymentLine is one payment line of an operation input that names no reference yet.
type pendingPaymentLine struct {
	index  int
	line   map[string]any
	method string
	amount decimal.Decimal
}

// settlePaymentLines checks each payment line of the operation input that names no reference. A line
// is accepted only when its method's provider confirms on request; any other method needs the till.
// With settle, each accepted line gets a payment intent, keyed by the request's idempotency key and
// the line index, and its id as the reference, so a replay of the same key reuses the same intent.
// It returns the request with the references filled in.
func (m aiModuleOperations) settlePaymentLines(ctx context.Context, execution policy.Execution, ref string, request contract.ExecutePublishedOperation, settle bool) (contract.ExecutePublishedOperation, error) {
	_, op, found := publishedOperationByRef(m.h.runtimeService(m.c, m.c.GetString("site_name")), ref)
	if !found || op.published.Payment == nil {
		return request, nil
	}
	binding := op.published.Payment
	var input map[string]any
	// A non-object input leaves input nil, so it has no payment lines; the operation's input check refuses it.
	_ = json.Unmarshal(request.Input, &input)
	pending, err := pendingPaymentLines(binding, input)
	if err != nil {
		return request, err
	}
	if len(pending) == 0 {
		return request, nil
	}
	svc := m.h.paymentService(m.c) //nolint:contextcheck // the request context travels in m.c
	if svc == nil {
		return request, ai.OperationError{Code: "payment.not_configured", Message: "Payments are not configured"}
	}
	for _, line := range pending {
		name, refusal := immediateProviderFor(svc, line.method)
		if refusal != nil {
			return request, refusal
		}
		if !settle {
			continue
		}
		intentID, intentErr := m.requestLineIntent(ctx, svc, execution, paymentIntentRequest{
			provider: name, sourceDoctype: binding.SourceDoctype, currency: stringField(input, binding.CurrencyField),
			key: fmt.Sprintf("%s-line-%d", request.Meta.IdempotencyKey, line.index), amount: line.amount,
		})
		if intentErr != nil {
			return request, intentErr
		}
		line.line[binding.ReferenceField] = intentID
	}
	if !settle {
		return request, nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return request, ai.OperationError{Code: "validation.invalid_input", Message: "Input must be an object of field values."}
	}
	request.Input = encoded
	return request, nil
}

// paymentIntentRequest is one payment intent a confirmed pack action needs for one payment line.
type paymentIntentRequest struct {
	provider, sourceDoctype, currency, key string
	amount                                 decimal.Decimal
}

// requestLineIntent requests the payment intent of one line and returns its id. The intent must be confirmed
// at once; a refusal is an OperationError the model can read.
func (m aiModuleOperations) requestLineIntent(ctx context.Context, svc *payment.Service, execution policy.Execution, req paymentIntentRequest) (string, error) {
	intent, err := svc.Request(ctx, m.c.GetString("site_name"), paymentActor(execution), payment.RequestInput{
		Provider: req.provider, Amount: req.amount, Currency: req.currency,
		SourceDoctype: req.sourceDoctype, IdempotencyKey: req.key,
	})
	if err != nil {
		failure := operationFailureFrom(err)
		return "", ai.OperationError{Code: failure.Code, Message: failure.Message}
	}
	if intent.State != payment.StateConfirmed {
		return "", ai.OperationError{Code: "payment.not_confirmed", Message: "The payment was not confirmed."}
	}
	return intent.ID, nil
}

// pendingPaymentLines returns the lines of the input that name no reference, with their method and amount.
func pendingPaymentLines(binding *contract.PaymentOperationContract, input map[string]any) ([]pendingPaymentLine, error) {
	lines, _ := input[binding.LinesField].([]any)
	var out []pendingPaymentLine
	for i, raw := range lines {
		line, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if reference, _ := line[binding.ReferenceField].(string); reference != "" {
			continue
		}
		amount, err := decimal.NewFromString(fmt.Sprint(line[binding.AmountField]))
		if err != nil {
			return nil, ai.OperationError{Code: "validation.invalid_value", Message: "Payment amount is not a number.", Field: binding.AmountField}
		}
		method, _ := line[ai.PaymentMethodField].(string)
		out = append(out, pendingPaymentLine{index: i, line: line, method: method, amount: amount})
	}
	return out, nil
}

// immediateProviderFor returns the provider name of a payment method when that provider confirms on
// request. The provider is named by the method in lower case, with spaces as underscores. A method with
// no such provider, or a provider that waits for the customer, needs the till.
func immediateProviderFor(svc *payment.Service, method string) (string, error) {
	name := strings.ToLower(strings.Join(strings.Fields(method), "_"))
	provider, ok := svc.Providers.Get(name)
	if imm, isImmediate := provider.(payment.ImmediateProvider); !ok || !isImmediate || !imm.ConfirmsOnRequest() {
		return "", ai.OperationError{Code: "payment.needs_till", Message: "this payment method needs the till", Field: ai.PaymentMethodField}
	}
	return name, nil
}

// paymentActor is the identity a payment intent is recorded for: the subject user, else the principal.
func paymentActor(execution policy.Execution) string {
	actor := execution.Actor()
	if actor.SubjectUserID != "" {
		return actor.SubjectUserID
	}
	return actor.PrincipalID
}

func stringField(input map[string]any, name string) string {
	value, _ := input[name].(string)
	return value
}

// Preview checks the payment lines that name no reference, then runs the operation as a preview. The
// lines are settled only at Run, under the confirm, so a preview requests no payment intent.
func (m aiModuleOperations) Preview(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, ref string, request contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	if _, err := m.settlePaymentLines(ctx, execution, ref, request, false); err != nil {
		return contract.OperationReceipt{}, err
	}
	return m.execute(ctx, publishedOperationRun{Execution: execution, Scope: scope, Ref: ref, Request: request, Preview: true})
}

// execute is the one path of Run and Preview: the same operation core, with the run's options.
func (m aiModuleOperations) execute(ctx context.Context, run publishedOperationRun) (contract.OperationReceipt, error) {
	if err := run.Request.Validate(); err != nil {
		return contract.OperationReceipt{}, ai.OperationError{Code: "validation.invalid_operation_request", Message: "Invalid published operation request"}
	}
	run.Services = m.h.runtimeService(m.c, m.c.GetString("site_name"))
	run.Roles = m.h.siteRegistry(m.c)
	receipt, err := runPublishedOperation(ctx, run)
	if err != nil {
		failure := operationFailureFrom(err)
		field, _ := failure.Details["field"].(string)
		return contract.OperationReceipt{}, ai.OperationError{Code: failure.Code, Message: failure.Message, Field: field}
	}
	return receipt, nil
}
