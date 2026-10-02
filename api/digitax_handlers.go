package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/secret"
	"github.com/gin-gonic/gin"
)

// HandleDigiTaxWebhook receives DigiTax's asynchronous sale.sync callback.
// It is intentionally registered on the public route group: DigiTax cannot
// maintain a Kora session or CSRF token. Authentication is done with the
// site secret digitax_webhook_secret in either the X-DigiTax-Webhook-Secret
// header or the token query parameter.
func (h *Handler) HandleDigiTaxWebhook(c *gin.Context) {
	site := c.GetString("site_name")
	db := h.queryDB(c)
	if site == "" || db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "site database unavailable"})
		return
	}

	expected, err := secret.NewStore(db, h.siteDialect(c)).Get(site, "digitax_webhook_secret")
	if err != nil || expected == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "DigiTax webhook secret is not configured"})
		return
	}
	provided := c.GetHeader("X-DigiTax-Webhook-Secret")
	if provided == "" {
		provided = c.Query("token")
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook secret"})
		return
	}

	raw, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook body"})
		return
	}
	var envelope struct {
		Data  map[string]any `json:"data"`
		Event string         `json:"event"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Event == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid DigiTax webhook body"})
		return
	}
	if envelope.Event != "sale.sync" {
		// Acknowledge other valid DigiTax events without mutating an invoice.
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "event": envelope.Event})
		return
	}

	traderInvoice := stringValue(envelope.Data["trader_invoice_number"])
	if traderInvoice == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trader_invoice_number is required"})
		return
	}

	queueStatus := strings.ToLower(stringValue(envelope.Data["queue_status"]))
	status := mapDigiTaxStatus(queueStatus)
	signedAt := parseDigiTaxTime(stringValue(envelope.Data["date"]), stringValue(envelope.Data["time"]))
	table := h.siteDialect(c).QuoteIdent("tabeTIMS Invoice")
	var invoiceName string
	var priorResponse sql.NullString
	lookup := fmt.Sprintf("SELECT name, submission_response FROM %s WHERE trader_invoice_number = ?", table)
	if err := db.QueryRowContext(c.Request.Context(), h.siteQuery(c, lookup), traderInvoice).Scan(&invoiceName, &priorResponse); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "eTIMS invoice not found", "trader_invoice_number": traderInvoice})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to find eTIMS invoice"})
		return
	}
	canonicalCallback, err := json.Marshal(envelope)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid DigiTax webhook body"})
		return
	}
	if priorResponse.Valid && sameDigiTaxCallback([]byte(priorResponse.String), canonicalCallback) {
		c.Header("X-Kora-Replay", "true")
		c.JSON(http.StatusOK, gin.H{"status": "received", "event": envelope.Event, "trader_invoice_number": traderInvoice})
		return
	}

	data, err := json.Marshal(map[string]any{
		"provider": "DigiTax", "trader_invoice_number": traderInvoice,
		"provider_status": queueStatus, "status": status,
		"provider_sale_id":  stringValue(envelope.Data["digitax_id"]),
		"etims_reference":   stringValue(envelope.Data["serial_number"]),
		"invoice_number":    stringValue(envelope.Data["invoice_number"]),
		"receipt_number":    stringValue(envelope.Data["receipt_number"]),
		"serial_number":     stringValue(envelope.Data["serial_number"]),
		"internal_data":     stringValue(envelope.Data["internal_data"]),
		"receipt_signature": stringValue(envelope.Data["receipt_signature"]),
		"etims_url":         stringValue(envelope.Data["etims_url"]),
		"sale_detail_url":   stringValue(envelope.Data["sale_detail_url"]),
		"callback_event":    envelope.Event, "webhook_received_at": time.Now().UTC(),
		"signed_at": signedAt, "customer_pin": stringValue(envelope.Data["customer_pin"]),
		"tax_summary":         jsonValue(envelope.Data["sales_tax_summary"]),
		"submission_response": string(raw),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare DigiTax callback"})
		return
	}
	// DigiTax callbacks are trusted service writes to fields intentionally
	// read-only in the interactive UI. Keep the privilege scoped to this adapter.
	c.Set("user", "digitax-callback")
	c.Set("user_role", doctype.AdminRole)
	c.Set("user_roles", []string{doctype.AdminRole})
	c.Set("auth_type", "service")
	fingerprint := sha256.Sum256(canonicalCallback)
	_, cerr := h.runKernelTrustedReadOnlyResourceMutationWithKey(c, kernel.CommandRecordUpdate, "eTIMS Invoice", invoiceName, data, fmt.Sprintf("digitax-callback:%x", fingerprint))
	if cerr != nil {
		if cerr.Type == contract.CodeIdempotencyKeyReused && digiTaxCallbackAlreadyStored(c, h, db, table, traderInvoice, canonicalCallback) {
			c.Header("X-Kora-Replay", "true")
			c.JSON(http.StatusOK, gin.H{"status": "received", "event": envelope.Event, "trader_invoice_number": traderInvoice})
			return
		}
		slog.Error("DigiTax callback kernel mutation failed", "site", site, "invoice", traderInvoice, "code", cerr.Type, "message", cerr.Message)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record DigiTax callback"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "received", "event": envelope.Event, "trader_invoice_number": traderInvoice})
}

func digiTaxCallbackAlreadyStored(c *gin.Context, h *Handler, db *sql.DB, table, traderInvoice string, callback []byte) bool {
	var stored sql.NullString
	query := fmt.Sprintf("SELECT submission_response FROM %s WHERE trader_invoice_number = ?", table)
	if err := db.QueryRowContext(c.Request.Context(), h.siteQuery(c, query), traderInvoice).Scan(&stored); err != nil {
		return false
	}
	return stored.Valid && sameDigiTaxCallback([]byte(stored.String), callback)
}

func sameDigiTaxCallback(stored, callback []byte) bool {
	var storedValue, callbackValue any
	if json.Unmarshal(stored, &storedValue) != nil || json.Unmarshal(callback, &callbackValue) != nil {
		return false
	}
	storedCanonical, err := json.Marshal(storedValue)
	if err != nil {
		return false
	}
	callbackCanonical, err := json.Marshal(callbackValue)
	return err == nil && string(storedCanonical) == string(callbackCanonical)
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func jsonValue(v any) any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func mapDigiTaxStatus(queueStatus string) string {
	switch queueStatus {
	case "completed":
		return "Accepted"
	case "failed":
		return "Failed"
	case "submitted":
		return "Submitted"
	case "pending":
		return "Pending"
	default:
		return "Pending"
	}
}

func parseDigiTaxTime(date, clock string) any {
	if date == "" || clock == "" {
		return nil
	}
	for _, layout := range []string{"02/01/2006 03:04:05 pm", "02/01/2006 03:04:05 PM"} {
		if parsed, err := time.ParseInLocation(layout, date+" "+clock, time.Local); err == nil {
			return parsed
		}
	}
	return nil
}
