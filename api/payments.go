package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/payment"
	"github.com/asenawritescode/kora/payment/manual"
	"github.com/asenawritescode/kora/payment/mpesa"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/secret"
)

// PaymentServiceFactory builds the payment service for one site database. The
// engine installs the production wiring; the default registers cash only.
var PaymentServiceFactory = func(database *sql.DB, dialect kdb.Dialect) *payment.Service {
	return &payment.Service{Store: &payment.SQLStore{DB: database, Dialect: dialect}, Providers: payment.NewRegistry()}
}

type paymentRequestBody struct {
	Provider      string `json:"provider"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	Contact       string `json:"contact"`
	SourceDoctype string `json:"source_doctype"`
	// OperationRef names the published payment-guarded operation the intent is
	// for. A caller holding that operation's write grant may then create the
	// intent without create permission on the source DocType.
	OperationRef string `json:"operation_ref"`
	// PaymentAccount and Reference are for the manual provider: the account the
	// customer paid and the confirmation code they showed.
	PaymentAccount string `json:"payment_account"`
	Reference      string `json:"reference"`
}

func paymentView(it payment.Intent) map[string]any {
	account := ""
	if it.Provider == manual.Name {
		account, _, _ = manual.Split(it.ProviderRequestID)
	}
	return map[string]any{
		"payment_account": account, "verification": manual.Verification(it.Provider),
		"id": it.ID, "provider": it.Provider, "state": it.State, "amount": it.Amount.String(),
		"currency": it.Currency, "contact": it.Contact, "source_doctype": it.SourceDoctype,
		"provider_request_id": it.ProviderRequestID, "provider_reference": it.ProviderReference,
		"failure_reason": it.FailureReason, "created_at": it.CreatedAt, "updated_at": it.UpdatedAt,
	}
}

func (h *Handler) paymentService(c *gin.Context) *payment.Service {
	database, _ := c.Get("site_db")
	sqlDB, _ := database.(*sql.DB)
	if sqlDB == nil {
		return nil
	}
	svc := PaymentServiceFactory(sqlDB, h.siteDialect(c))
	if _, ok := svc.Providers.Get("mpesa"); !ok {
		mp := mpesa.New(mpesa.Config{Site: c.GetString("site_name"), Secrets: secret.NewStore(sqlDB, h.siteDialect(c)),
			CallbackBase: requestBaseURL(c.Request)})
		if mp.Enabled() {
			_ = svc.Providers.Register(mp)
		}
	}
	if _, ok := svc.Providers.Get(manual.Name); !ok {
		// Recording against configured accounts exists only where the tenant
		// defines the Payment Account DocType.
		if dt := h.siteRegistry(c).Get("Payment Account"); dt != nil {
			_ = svc.Providers.Register(manual.Provider{Accounts: paymentAccounts{tx: h.siteTx(c), dt: dt}})
		}
	}
	return svc
}

// paymentAccounts reads Payment Account documents for the manual provider.
type paymentAccounts struct {
	tx *orm.TxManager
	dt *doctype.DocType
}

func (p paymentAccounts) Account(_ context.Context, name string) (manual.Account, bool, error) {
	doc, err := p.tx.GetDoc(p.dt, name, "")
	if errors.Is(err, orm.ErrNotFound) {
		return manual.Account{}, false, nil
	}
	if err != nil {
		return manual.Account{}, false, err
	}
	return manual.Account{Enabled: flagSet(doc.Get("enabled")), Currency: doc.GetString("currency")}, true, nil
}

// flagSet reads a Check column as the drivers return it: a bool, a number or
// the text "1"/"true".
func flagSet(v any) bool {
	switch n := v.(type) {
	case bool:
		return n
	case int:
		return n != 0
	case int64:
		return n != 0
	case float64:
		return n != 0
	case []byte:
		return flagSet(string(n))
	case string:
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "1", "true", "t", "yes":
			return true
		}
	}
	return false
}

func paymentError(c *gin.Context, err error) {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		internalError(c, "payment failed", err)
		return
	}
	switch ce.Type {
	case contract.CodeValidationFailed:
		badRequestError(c, "payment.invalid", ce.Message, nil)
	case contract.CodeConflict:
		conflictError(c, "payment.idempotency_conflict", ce.Message, nil)
	case contract.CodeNotFound:
		notFoundError(c, "payment.not_found", ce.Message, nil)
	default:
		internalError(c, "payment failed", err)
	}
}

// holdsPaymentOperationGrant reports whether the caller holds the write grant
// of any published operation whose payment binding is for sourceDoctype.
func (h *Handler) holdsPaymentOperationGrant(c *gin.Context, execution policy.Execution, sourceDoctype string) bool {
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.LoadModuleSnapshot == nil {
		return false
	}
	snapshot, err := services.LoadModuleSnapshot()
	if err != nil {
		return false
	}
	scope := businessScopeFromRequest(c, execution)
	for _, module := range snapshot.Modules {
		for _, published := range module.PublishedOperations {
			if published.Payment == nil || published.Payment.SourceDoctype != sourceDoctype {
				continue
			}
			if _, err := authorizePublishedOperation(execution, h.siteRegistry(c), module.ID, published, scope); err == nil {
				return true
			}
		}
	}
	return false
}

// denyPaymentIntent authorizes creating an intent for sourceDoctype and writes
// any refusal. Callers with create permission on the source DocType keep that
// path. Otherwise the intent must name a published operation whose payment
// binding is for the same source DocType, and the caller must hold that
// operation's write grant.
func (h *Handler) denyPaymentIntent(c *gin.Context, sourceDoctype, operationRef string) bool {
	_, denial := permScope(c, h.siteRegistry(c), sourceDoctype, "create")
	execution, valid := requestExecution(c)
	if operationRef == "" || !valid {
		return writePermDenial(c, denial)
	}
	_, op, found := publishedOperationByRef(h.runtimeService(c, c.GetString("site_name")), operationRef)
	switch {
	case !found:
		notFoundError(c, "payment.operation_not_found", "Published operation not found", nil)
		return true
	case op.published.Payment == nil:
		badRequestError(c, "payment.operation_not_payment_guarded", "The operation has no payment binding", nil)
		return true
	case op.published.Payment.SourceDoctype != sourceDoctype:
		badRequestError(c, "payment.operation_source_mismatch", "The operation's payment binding is for a different source_doctype", nil)
		return true
	case denial == nil:
		return false
	}
	scope := businessScopeFromRequest(c, execution)
	if contract.ValidateResourceBusinessScope(scope, op.published.ScopePolicy) != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation scope is not permitted", nil)
		return true
	}
	if _, err := authorizePublishedOperation(execution, h.siteRegistry(c), op.owner, op.published, scope); err != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation access is not granted", nil)
		return true
	}
	return false
}

// denyPaymentRead authorizes reading an intent: create permission on its source
// DocType, or the write grant of any published operation bound to it.
func (h *Handler) denyPaymentRead(c *gin.Context, sourceDoctype string) bool {
	_, denial := permScope(c, h.siteRegistry(c), sourceDoctype, "create")
	if denial == nil {
		return false
	}
	if execution, valid := requestExecution(c); valid && h.holdsPaymentOperationGrant(c, execution, sourceDoctype) {
		return false
	}
	return writePermDenial(c, denial)
}

func writePermDenial(c *gin.Context, denial *permDenial) bool {
	if denial == nil {
		return false
	}
	c.JSON(denial.status, ErrorResponse{Error: map[string]string{"message": denial.message}})
	return true
}

// HandlePaymentRequest serves POST /payments.
func (h *Handler) HandlePaymentRequest(c *gin.Context) {
	svc := h.paymentService(c)
	if svc == nil {
		notFoundError(c, "payment.not_configured", "Payments are not configured", nil)
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		badRequestError(c, "payment.idempotency_key_required", "Idempotency-Key header is required", nil)
		return
	}
	var body paymentRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		badRequestError(c, "payment.invalid", "Invalid payment request body", nil)
		return
	}
	amount, err := decimal.NewFromString(body.Amount)
	if err != nil || body.SourceDoctype == "" {
		badRequestError(c, "payment.invalid", "amount and source_doctype are required", nil)
		return
	}
	if h.denyPaymentIntent(c, body.SourceDoctype, body.OperationRef) {
		return
	}
	actor := ""
	if execution, ok := requestExecution(c); ok {
		a := execution.Actor()
		if actor = a.SubjectUserID; actor == "" {
			actor = a.PrincipalID
		}
	}
	it, err := svc.Request(c.Request.Context(), c.GetString("site_name"), actor, payment.RequestInput{
		Provider: body.Provider, Amount: amount, Currency: body.Currency, Contact: body.Contact,
		SourceDoctype: body.SourceDoctype, IdempotencyKey: key,
		Account: body.PaymentAccount, Reference: body.Reference,
	})
	if err != nil {
		paymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": paymentView(it)})
}

// HandlePaymentGet serves GET /payments/:id.
func (h *Handler) HandlePaymentGet(c *gin.Context) {
	svc := h.paymentService(c)
	if svc == nil {
		notFoundError(c, "payment.not_configured", "Payments are not configured", nil)
		return
	}
	it, err := svc.Get(c.Request.Context(), c.GetString("site_name"), c.Param("id"))
	if err != nil {
		paymentError(c, err)
		return
	}
	if h.denyPaymentRead(c, it.SourceDoctype) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": paymentView(it)})
}

// HandlePaymentCallback serves POST /webhooks/payments/:provider/:token. The
// token is validated by the provider; bodies and tokens are never logged.
func (h *Handler) HandlePaymentCallback(c *gin.Context) {
	svc := h.paymentService(c)
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "site database unavailable"})
		return
	}
	site := c.GetString("site_name")
	p, ok := svc.Providers.Get(c.Param("provider"))
	cb, isCB := p.(payment.CallbackProvider)
	if !ok || !isCB {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown payment provider"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid callback body"})
		return
	}
	conf, err := cb.ParseCallback(c.Request.Context(), site, c.Param("token"), body)
	switch {
	case errors.Is(err, payment.ErrInvalidCallbackToken):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid callback credential"})
		return
	case err != nil:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid callback"})
		return
	}
	_, res, err := svc.Confirm(c.Request.Context(), site, conf)
	if errors.Is(err, payment.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}
	if err != nil {
		internalError(c, "payment callback failed", err)
		return
	}
	if res == payment.ConfirmConflict {
		slog.Warn("payment callback conflicts with settled intent", "site", site, "provider", conf.Provider)
	}
	c.JSON(http.StatusOK, gin.H{"ResultCode": 0, "ResultDesc": "Accepted"})
}

func requestBaseURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}
