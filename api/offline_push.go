package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/kernel"
	"github.com/gin-gonic/gin"
)

const offlinePushMaxOperations = 50

// HandleOfflinePush executes each record command through the same transactional
// kernel used by online writes. A failed operation does not block unrelated
// operations in the batch. Replaying an operation uses the kernel's durable
// idempotency receipt, including after a lost HTTP response.
func (h *Handler) HandleOfflinePush(c *gin.Context) {
	if _, ok := requestExecution(c); !ok {
		h.writeKernelError(c, contract.NewError(contract.CodeUnauthenticated, "verified execution identity is required"))
		return
	}
	var req contract.OfflinePushRequestV1
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Operations) == 0 || len(req.Operations) > offlinePushMaxOperations {
		badRequestError(c, "validation.invalid_offline_batch", "Expected 1 to 50 offline operations", nil)
		return
	}
	seen := make(map[string]bool, len(req.Operations))
	for _, op := range req.Operations {
		if strings.TrimSpace(op.ID) == "" || len(op.ID) > 256 || strings.TrimSpace(op.IdempotencyKey) == "" || len(op.IdempotencyKey) > 256 || seen[op.ID] || strings.TrimSpace(op.Doctype) == "" || len(op.Payload) == 0 || !json.Valid(op.Payload) {
			badRequestError(c, "validation.invalid_offline_operation", "Offline operation identity or payload is invalid", nil)
			return
		}
		if op.Command != kernel.CommandRecordCreate && op.Command != kernel.CommandRecordUpdate || op.Command == kernel.CommandRecordUpdate && (strings.TrimSpace(op.Name) == "" || strings.TrimSpace(op.ExpectedRevision) == "") {
			badRequestError(c, "validation.unsupported_offline_command", "Only versioned record create and update are supported", nil)
			return
		}
		seen[op.ID] = true
	}
	receipts := make([]contract.OfflinePushReceiptV1, 0, len(req.Operations))
	for _, op := range req.Operations {
		payload, _ := json.Marshal(map[string]any{"doctype": op.Doctype, "name": op.Name, "data": op.Payload})
		result, cerr := h.executeKernelOperation(c, op.Command, kernelRequest{IdempotencyKey: op.IdempotencyKey, ExpectedVersion: op.ExpectedRevision, Payload: payload})
		receipt := contract.OfflinePushReceiptV1{OperationID: op.ID}
		if cerr != nil {
			receipt.Status = "rejected"
			if cerr.Type == contract.CodeConflict || cerr.Type == contract.CodeStaleSubject {
				receipt.Status = "conflict"
			} else if cerr.Type == contract.CodeUnauthenticated || cerr.Type == contract.CodeDependencyUnavailable || cerr.Type == contract.CodeDeadlineExceeded || cerr.Type == contract.CodeResourceExhausted || cerr.Type == contract.CodeInternal || cerr.Type == contract.CodeOutcomeUnknown {
				receipt.Status = "deferred"
			}
			receipt.Error = &contract.OfflineProblemV1{Code: string(cerr.Type), Message: cerr.Message}
		} else {
			var data kernel.ResultData
			if err := json.Unmarshal(result.Data, &data); err != nil || data.Document == nil {
				receipt.Status = "deferred"
				receipt.Error = &contract.OfflineProblemV1{Code: "INVALID_RECEIPT", Message: "Kernel did not return a record"}
			} else {
				receipt.Status = "acked"
				receipt.Replayed = result.Replayed
				receipt.Record, _ = json.Marshal(data.Document)
			}
		}
		receipts = append(receipts, receipt)
	}
	c.JSON(http.StatusOK, Response{Data: contract.OfflinePushResponseV1{Receipts: receipts}})
}
