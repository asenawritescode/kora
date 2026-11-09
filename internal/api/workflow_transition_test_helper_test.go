//go:build integration

package api

import (
	"encoding/json"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/gin-gonic/gin"
)

// runKernelWorkflowTransition invokes the kernel command directly for integration
// coverage. It deliberately lives in test code: user-facing transitions now run
// through the published workflow service, not the retired resource subroute.
func (h *Handler) runKernelWorkflowTransition(c *gin.Context, doctypeName, name, action string, expectedRevision uint64, expectedConfigRevision ...uint64) (*doctype.Document, *contract.Error) {
	var configRevision uint64
	if len(expectedConfigRevision) > 0 {
		configRevision = expectedConfigRevision[0]
	}
	payload, err := json.Marshal(kernel.WorkflowStatusTransitionPayload{
		Doctype: doctypeName, Name: name, Action: action,
	})
	if err != nil {
		return nil, contract.NewError(contract.CodeInternal, "encoding workflow transition failed")
	}
	result, commandErr := h.executeKernelOperation(c, kernel.CommandWorkflowStatusTransition, kernelRequest{
		IdempotencyKey: c.GetHeader("Idempotency-Key"), Payload: payload, ExpectedConfigRevision: configRevision,
	})
	if commandErr != nil {
		c.Set("test_workflow_transition_error", commandErr.Message)
		return nil, commandErr
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(result.Data, &operation); err != nil {
		return nil, contract.NewError(contract.CodeInternal, "decoding workflow result failed")
	}
	doc := orm.DocumentFromMap(h.siteRegistry(c), doctypeName, operation.Document)
	if doc == nil {
		return nil, contract.NewError(contract.CodeInternal, "kernel did not return the workflow document")
	}
	return doc, nil
}
