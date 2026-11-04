package engine

import (
	"encoding/json"
	"testing"
)

func TestLocalPublishedCommandPayloadWrapsPublishedInput(t *testing.T) {
	got := localPublishedCommandPayload([]byte(`{"title":"invoice"}`))
	var decoded struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("wrapped payload is invalid JSON: %v", err)
	}
	if decoded.Data["title"] != "invoice" {
		t.Fatalf("wrapped payload data = %#v", decoded.Data)
	}
}
