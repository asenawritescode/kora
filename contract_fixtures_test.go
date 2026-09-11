package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestOrganizationalRuntimeFixtureIsValid(t *testing.T) {
	data, err := os.ReadFile("contract-fixtures/organizational-runtime-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version       string   `json:"version"`
		ResourceKinds []string `json:"resource_kinds"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != "1" || len(fixture.ResourceKinds) < 17 {
		t.Fatalf("unexpected fixture: %+v", fixture)
	}
}
