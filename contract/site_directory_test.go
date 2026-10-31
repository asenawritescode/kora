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

func TestSiteDirectoryV2PreservesDecimalRevisionsAndDeploymentAssignment(t *testing.T) {
	data, err := os.ReadFile("../contract-fixtures/site-directory-v2.json")
	if err != nil {
		t.Fatal("read Site Directory v2 fixture:", err)
	}
	var entry SiteDirectoryEntryV2
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal("decode Site Directory v2 fixture:", err)
	}
	if err := ValidateSiteDirectoryEntryV2(entry); err != nil {
		t.Fatalf("versioned directory fixture rejected: %v", err)
	}
	if entry.ConfigRevision != "9007199254740993" || entry.DirectoryRevision != "18446744073709551615" {
		t.Fatalf("fixture revisions lost decimal precision: %+v", entry)
	}
	encodedFixture, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var fixtureRoundTrip SiteDirectoryEntryV2
	if err := json.Unmarshal(encodedFixture, &fixtureRoundTrip); err != nil {
		t.Fatal(err)
	}
	if fixtureRoundTrip.ConfigRevision != entry.ConfigRevision || fixtureRoundTrip.DirectoryRevision != entry.DirectoryRevision {
		t.Fatalf("fixture round-trip changed decimal revisions: %+v", fixtureRoundTrip)
	}

	sample := SiteDirectoryEntryV2{
		Version: 2, SiteID: "tenant-a", Aliases: []string{"shop.example.test"}, Status: "active",
		ConfigRevision: "9007199254740993", DirectoryRevision: "18446744073709551615",
		DeploymentID: "deployment-west", DataPlaneRef: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := ValidateSiteDirectoryEntryV2(sample); err != nil {
		t.Fatalf("valid deployment directory entry rejected: %v", err)
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip SiteDirectoryEntryV2
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.ConfigRevision != sample.ConfigRevision || roundTrip.DirectoryRevision != sample.DirectoryRevision || roundTrip.DeploymentID != sample.DeploymentID {
		t.Fatalf("v2 directory round-trip changed assignment or decimal revisions: %#v", roundTrip)
	}
}

func TestValidateSiteDirectoryV2RejectsInvalidAssignmentAndRevisions(t *testing.T) {
	valid := SiteDirectoryEntryV2{
		Version: 2, SiteID: "tenant-a", Status: "active", ConfigRevision: "1",
		DirectoryRevision: "2", DeploymentID: "deployment-a",
	}
	for _, test := range []struct {
		name string
		edit func(*SiteDirectoryEntryV2)
	}{
		{name: "wrong version", edit: func(entry *SiteDirectoryEntryV2) { entry.Version = 1 }},
		{name: "empty assignment", edit: func(entry *SiteDirectoryEntryV2) { entry.DeploymentID = " " }},
		{name: "unsafe revision", edit: func(entry *SiteDirectoryEntryV2) { entry.DirectoryRevision = "01" }},
		{name: "duplicate alias", edit: func(entry *SiteDirectoryEntryV2) { entry.Aliases = []string{"shop.example.test", "shop.example.test"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := valid
			test.edit(&entry)
			if err := ValidateSiteDirectoryEntryV2(entry); err == nil {
				t.Fatal("invalid deployment directory entry was accepted")
			}
		})
	}
}
