package contract

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSiteDirectoryV1FixtureMatchesGoContract(t *testing.T) {
	data, err := os.ReadFile("../contract-fixtures/site-directory-v1.json")
	if err != nil {
		t.Fatalf("read Site Directory fixture: %v", err)
	}
	var descriptor SiteDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		t.Fatalf("decode Site Directory fixture: %v", err)
	}
	if descriptor.Version != SiteDirectoryVersion || descriptor.SiteID == "" || descriptor.Status == "" || len(descriptor.Aliases) == 0 {
		t.Fatalf("invalid Site Directory v1 fixture: %#v", descriptor)
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("encode Site Directory contract: %v", err)
	}
	var roundTrip SiteDescriptor
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatalf("decode Site Directory round trip: %v", err)
	}
	if roundTrip.SiteID != descriptor.SiteID || roundTrip.ConfigRevision != descriptor.ConfigRevision || roundTrip.Version != descriptor.Version {
		t.Fatalf("contract round trip changed identity/revision: %#v -> %#v", descriptor, roundTrip)
	}
}
