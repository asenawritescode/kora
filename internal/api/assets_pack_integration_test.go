//go:build integration

package api

import "testing"

// The Assets pack as shipped, on real MySQL: a task cannot be marked Done
// without a date, an assignment cannot run backwards, a retired asset's tag is frozen.
func TestAssetsRegisterAndMaintenanceWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "assets")
	c := newPackClient(t, handler, db, reg, site)

	category := c.create("Asset Category", `{"category_name":"Vehicles"}`)
	asset := c.create("Asset", `{"asset_tag":"A-1","asset_name":"Delivery van","category":"`+category+`","purchase_date":"2026-01-10","purchase_cost":2500000}`)
	if got := c.value("Asset", asset, "status"); got != "In use" {
		t.Fatalf("new asset status = %q, want In use", got)
	}
	if !c.refused(httpMethodPost, "Asset", "", `{"asset_tag":"A-1","asset_name":"Other van"}`) {
		t.Fatal("a second asset with the same tag was accepted")
	}

	task := c.create("Maintenance Task", `{"reference":"MT-1","asset":"`+asset+`","description":"Oil change","due_date":"2026-12-01"}`)
	if got := c.value("Maintenance Task", task, "status"); got != "Planned" {
		t.Fatalf("new task status = %q, want Planned", got)
	}
	if !c.refused(httpMethodPut, "Maintenance Task", task, `{"status":"Done"}`) {
		t.Fatal("a task was marked Done without a done-on date")
	}
	if got := c.value("Maintenance Task", task, "status"); got != "Planned" {
		t.Fatalf("task status after a refused change = %q, want Planned", got)
	}
	if c.refused(httpMethodPut, "Maintenance Task", task, `{"status":"Done","done_on":"2026-12-02","cost":8000}`) {
		t.Fatal("a Done task with a done-on date was refused")
	}

	user := "driver@example.com"
	if !c.refused(httpMethodPost, "Asset Assignment", "", `{"asset":"`+asset+`","assigned_to":"`+user+`","from_date":"2026-06-10","to_date":"2026-06-01"}`) {
		t.Fatal("an assignment ending before it starts was accepted")
	}
	c.create("Asset Assignment", `{"asset":"`+asset+`","assigned_to":"`+user+`","from_date":"2026-06-01","to_date":"2026-06-10"}`)

	if c.refused(httpMethodPut, "Asset", asset, `{"status":"Retired"}`) {
		t.Fatal("retiring an asset was refused")
	}
	if !c.refused(httpMethodPut, "Asset", asset, `{"asset_tag":"A-99"}`) {
		t.Fatal("the tag of a retired asset was changed")
	}
	if got := c.value("Asset", asset, "asset_tag"); got != "A-1" {
		t.Fatalf("tag after a refused change = %q, want A-1", got)
	}
}
