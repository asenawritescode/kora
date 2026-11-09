// Package mpesa holds provider-pure Safaricom M-Pesa helpers. It has no
// knowledge of tenant DocTypes or storage.
package mpesa

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrMissingCheckoutRequestID reports a callback without a CheckoutRequestID.
var ErrMissingCheckoutRequestID = errors.New("CheckoutRequestID is required")

// CallbackEnvelope is the top-level STK callback body.
type CallbackEnvelope struct {
	Body struct {
		STKCallback STKCallback `json:"stkCallback"`
	} `json:"Body"`
}

// STKCallback is the STK push result delivered by Safaricom.
type STKCallback struct {
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

// Receipt returns the MpesaReceiptNumber metadata item, or "".
func (c STKCallback) Receipt() string {
	for _, item := range c.CallbackMetadata.Item {
		if item.Name == "MpesaReceiptNumber" {
			return fmt.Sprint(item.Value)
		}
	}
	return ""
}

// TokenValid compares the provided and expected callback tokens in constant
// time. Empty provided tokens are never valid.
func TokenValid(provided, expected string) bool {
	return provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// ParseCallback decodes a callback body into its outcome fields.
func ParseCallback(body []byte) (checkoutRequestID string, success bool, receipt, resultDesc string, err error) {
	var env CallbackEnvelope
	if err = json.Unmarshal(body, &env); err != nil {
		return "", false, "", "", fmt.Errorf("decode M-Pesa callback: %w", err)
	}
	cb := env.Body.STKCallback
	if cb.CheckoutRequestID == "" {
		return "", false, "", "", ErrMissingCheckoutRequestID
	}
	success = cb.ResultCode == 0
	if success {
		receipt = cb.Receipt()
	}
	return cb.CheckoutRequestID, success, receipt, cb.ResultDesc, nil
}
