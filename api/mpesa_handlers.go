package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/gin-gonic/gin"
)

// HandleMPesaSTKCallback accepts Safaricom's asynchronous STK callback.
// The callback is site-scoped by the public route group and matched by the
// provider CheckoutRequestID. It is intentionally idempotent: once an
// operation is terminal, repeated callbacks are recorded but do not mutate
// the payment again.
func (h *Handler) HandleMPesaSTKCallback(c *gin.Context) {
	var envelope mpesaCallbackEnvelope
	if err := c.ShouldBindJSON(&envelope); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid M-Pesa callback"})
		return
	}
	callback := envelope.Body.STKCallback
	if callback.CheckoutRequestID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "CheckoutRequestID is required"})
		return
	}

	db := h.queryDB(c)
	if db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "site database unavailable"})
		return
	}
	// This public provider callback has no Kora session. The route currently
	// trusts deployment/provider ingress controls to authenticate the sender;
	// the service actor here is attribution, not proof of callback authenticity.
	c.Set("user", "mpesa-callback")
	c.Set("user_role", doctype.AdminRole)
	c.Set("user_roles", []string{doctype.AdminRole})
	c.Set("auth_type", "service")
	operationName, err := externalOperationName(h, c, db, callback.CheckoutRequestID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment operation not found"})
		return
	}

	reg := h.siteRegistry(c)
	operationDT := reg.Get("External Operation")
	if operationDT == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "External Operation is not configured"})
		return
	}
	tm := h.siteTx(c)
	operation, err := tm.GetDoc(operationDT, operationName, "")
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment operation not found"})
		return
	}

	previousStatus := operation.GetString("status")
	if isTerminalPaymentStatus(previousStatus) {
		// Keep callback evidence, but do not label contradictory provider data as
		// a retry of the result already committed for this operation.
		responseStatus, eventErr := h.recordTerminalMPesaCallback(c, operation, callback, envelope)
		if eventErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record terminal payment callback"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": responseStatus, "operation": operationName})
		return
	}

	newStatus := "Failed"
	if callback.ResultCode == 0 {
		newStatus = "Succeeded"
	}
	operation.Set("status", newStatus)
	operation.Set("error_message", callback.ResultDesc)
	if callback.ResultCode == 0 {
		operation.Set("error_message", "")
		operation.Set("completed_at", time.Now().UTC())
		if receipt := callbackReceipt(callback); receipt != "" {
			operation.Set("provider_reference", receipt)
		}
	}
	operationData, err := transactionDocumentData(operationDT, operation)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare payment operation"})
		return
	}
	bundle := kernel.RecordMutationBundlePayload{
		Doctype: operationDT.Name,
		Records: []kernel.RecordMutationBundleItem{{
			Key: "operation", Operation: "update", Doctype: operationDT.Name,
			Name: operationName, Data: operationData,
		}},
	}
	if paymentDT := reg.Get("Payment"); paymentDT != nil {
		if paymentName, lookupErr := linkedPaymentName(h, c, db, operationName); lookupErr == nil && paymentName != "" {
			paymentData, encodeErr := json.Marshal(map[string]any{
				"status": newStatus, "provider_reference": operation.Get("provider_reference"),
			})
			if encodeErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare payment record"})
				return
			}
			bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{
				Key: "payment", Operation: "update", Doctype: paymentDT.Name, Name: paymentName, Data: paymentData,
			})
		}
	}
	eventDT := reg.Get("External Operation Event")
	if eventDT != nil {
		eventKey := "mpesa-callback:" + callback.CheckoutRequestID
		eventData, encodeErr := externalOperationEventData(eventDT, operation, "Inbound", "Callback", previousStatus, newStatus, envelope, nil, "Processed", "", eventKey, time.Now().UTC())
		if encodeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare callback event"})
			return
		}
		bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{
			Key: "event", Doctype: eventDT.Name, Data: eventData,
		})
	}
	if _, _, cerr := h.runKernelMutationBundle(c, bundle, "mpesa-callback:"+callback.CheckoutRequestID); cerr != nil {
		// Two deliveries may both observe a pending operation before either
		// bundle commits. Their request timestamps make the serialized payloads
		// differ, so idempotency correctly rejects the second payload. Resolve
		// that race from durable state: acknowledge only if the winner committed
		// the same terminal result, then persist ordinary duplicate evidence.
		current, reloadErr := tm.GetDoc(operationDT, operationName, "")
		if reloadErr == nil && isTerminalPaymentStatus(current.GetString("status")) {
			responseStatus, eventErr := h.recordTerminalMPesaCallback(c, current, callback, envelope)
			if eventErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record terminal payment callback"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"status": responseStatus, "operation": operationName})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process payment callback"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "received", "operation": operationName, "payment_status": newStatus})
}

func (h *Handler) recordTerminalMPesaCallback(c *gin.Context, operation *doctype.Document, callback mpesaSTKCallback, envelope mpesaCallbackEnvelope) (string, *contract.Error) {
	status, processingStatus, message := "ignored", "Ignored", "callback result conflicts with terminal operation result"
	if mpesaCallbackMatchesTerminal(operation, callback) {
		status, processingStatus, message = "duplicate", "Duplicate", "terminal operation already processed"
	}
	terminalStatus := operation.GetString("status")
	err := h.recordExternalOperationEvent(c, operation, "Inbound", "Callback", terminalStatus, terminalStatus, envelope, nil, processingStatus, message)
	return status, err
}

func mpesaCallbackMatchesTerminal(operation *doctype.Document, callback mpesaSTKCallback) bool {
	if operation == nil {
		return false
	}
	if callback.ResultCode == 0 {
		receipt := callbackReceipt(callback)
		return operation.GetString("status") == "Succeeded" && (receipt == "" || operation.GetString("provider_reference") == receipt)
	}
	return operation.GetString("status") == "Failed" && operation.GetString("error_message") == callback.ResultDesc
}

type mpesaCallbackEnvelope struct {
	Body struct {
		STKCallback mpesaSTKCallback `json:"stkCallback"`
	} `json:"Body"`
}

type mpesaSTKCallback struct {
	MerchantRequestID string `json:"MerchantRequestID"`
	CheckoutRequestID string `json:"CheckoutRequestID"`
	ResultCode        int    `json:"ResultCode"`
	ResultDesc        string `json:"ResultDesc"`
	CallbackMetadata  struct {
		Item []struct {
			Name  string `json:"Name"`
			Value any    `json:"Value"`
		} `json:"Item"`
	} `json:"CallbackMetadata"`
}

func callbackReceipt(callback mpesaSTKCallback) string {
	for _, item := range callback.CallbackMetadata.Item {
		if item.Name == "MpesaReceiptNumber" {
			return fmt.Sprint(item.Value)
		}
	}
	return ""
}

func isTerminalPaymentStatus(status string) bool {
	return status == "Succeeded" || status == "Failed" || status == "Cancelled" || status == "Expired" || status == "Finalized"
}

func externalOperationName(h *Handler, c *gin.Context, database *sql.DB, requestID string) (string, error) {
	var operationName string
	query := fmt.Sprintf("SELECT name FROM %s WHERE provider_request_id = ? OR provider_reference = ? LIMIT 1", h.siteDialect(c).QuoteIdent("tabExternal Operation"))
	err := database.QueryRow(h.siteQuery(c, query), requestID, requestID).Scan(&operationName)
	return operationName, err
}

func linkedPaymentName(h *Handler, c *gin.Context, database *sql.DB, operationName string) (string, error) {
	var paymentName string
	query := fmt.Sprintf("SELECT name FROM %s WHERE external_operation = ? LIMIT 1", h.siteDialect(c).QuoteIdent("tabPayment"))
	row := database.QueryRow(h.siteQuery(c, query), operationName)
	if err := row.Scan(&paymentName); err != nil {
		return "", err
	}
	return paymentName, nil
}
