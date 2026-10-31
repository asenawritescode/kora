package api

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
)

func TestResourceProjectionExposesCanonicalOfflineVersion(t *testing.T) {
	dt := &doctype.DocType{Name: "Task"}
	doc := doctype.NewDocument(dt.Name)
	doc.Name = "TASK-1"
	modified := time.Date(2026, 10, 23, 12, 30, 0, 123000000, time.FixedZone("local", 3*60*60))
	doc.Set("modified", modified)
	projection := docToMap(doc, dt, nil, nil)
	if got, want := projection["_version"], kernel.CanonicalVersion(modified); got != want {
		t.Fatalf("offline version = %v, want %s", got, want)
	}
	if projection["name"] != doc.Name {
		t.Fatal("resource identity changed")
	}
}
