package api

import (
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
	"github.com/asenawritescode/kora/payment"
	"github.com/asenawritescode/kora/payment/mpesa"
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
}

func paymentView(it payment.Intent) map[string]any {
	return map[string]any{
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
	return svc
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
	if _, denied := checkPerm(c, h.siteRegistry(c), body.SourceDoctype, "create"); denied {
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
	if _, denied := checkPerm(c, h.siteRegistry(c), it.SourceDoctype, "create"); denied {
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
