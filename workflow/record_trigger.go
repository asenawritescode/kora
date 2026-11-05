package workflow

import (
	"encoding/json"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

// RecordTriggerEventPatterns are the outbox event types that can start
// record-triggered workflows: kernel change events, plus legacy record.*
// events written before each mutation had a single change event.
var RecordTriggerEventPatterns = []string{"kora.%.after!_insert", "kora.%.after!_save", "record.created", "record.updated"}

// RecordTriggerFromChangeEvent projects a kernel change event onto the record
// trigger contract. Writes without record identity, deletes, and updates that
// changed no field start no workflow.
func RecordTriggerFromChangeEvent(event contract.EventEnvelope) (contract.EventEnvelope, bool) {
	if event.Type == "record.created" || event.Type == "record.updated" {
		return event, true
	}
	kind := ""
	switch {
	case strings.HasSuffix(event.Type, ".after_insert"):
		kind = "record.created"
	case strings.HasSuffix(event.Type, ".after_save"):
		kind = "record.updated"
	default:
		return contract.EventEnvelope{}, false
	}
	var payload contract.RecordChangeEventData
	if err := json.Unmarshal(event.Data, &payload); err != nil || payload.Table == "" || payload.RecordID == "" || payload.Revision == "" {
		return contract.EventEnvelope{}, false
	}
	if kind == "record.updated" && len(payload.ChangedFields) == 0 {
		return contract.EventEnvelope{}, false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return contract.EventEnvelope{}, false
	}
	trigger := event
	trigger.Type = kind
	trigger.Data = data
	return trigger, true
}
