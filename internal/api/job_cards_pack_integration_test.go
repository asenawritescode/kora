//go:build integration

package api

import (
	"strconv"
	"strings"
	"testing"
)

func workshopStock(t *testing.T, c *packClient, product string) float64 {
	t.Helper()
	got, err := strconv.ParseFloat(c.value("Product", product, "stock_qty"), 64)
	if err != nil {
		t.Fatalf("stock of %s: %v", product, err)
	}
	return got
}

// The Workshop pack as shipped, on real MySQL: a vehicle, a job card that takes
// parts from stock, the refusals around it, and what editing a saved job card
// does (and does not do) to stock.
func TestWorkshopJobCardTakesPartsFromStockWorkflowOnMySQL(t *testing.T) {
	handler, db, reg, site := newPackLayersHandler(t, "pos-base", "job-cards")
	c := newPackClient(t, handler, db, reg, site)

	filter := c.create("Product", `{"reference":"W-FILTER","product_name":"Oil filter","selling_price":500,"stock_qty":10}`)
	pads := c.create("Product", `{"reference":"W-PADS","product_name":"Brake pads","selling_price":2500,"stock_qty":4}`)

	vehicle := c.create("Customer Vehicle", `{"registration":"KDA 123A","make":"Toyota","model":"Probox","year":2014,"owner_name":"Achieng","owner_phone":"0700000000"}`)
	if !c.refused(httpMethodPost, "Customer Vehicle", "", `{"registration":"KDA 123A"}`) {
		t.Fatal("a second vehicle with the same registration was accepted")
	}

	// Saving a job card takes its parts off stock, one change per product.
	card := c.create("Job Card", `{"reference":"JC-1","customer_vehicle":"`+vehicle+`","complaint":"Service","opened_on":"2026-12-01","promised_on":"2026-12-03","labour_charge":1500,`+
		`"parts":[{"product":"`+filter+`","quantity":1},{"product":"`+pads+`","quantity":2}]}`)
	if got := c.value("Job Card", card, "status"); got != "Booked" {
		t.Fatalf("job card status = %q, want Booked", got)
	}
	if got := workshopStock(t, c, filter); got != 9 {
		t.Fatalf("filter stock after the job card = %v, want 9", got)
	}
	if got := workshopStock(t, c, pads); got != 2 {
		t.Fatalf("pads stock after the job card = %v, want 2", got)
	}

	// Parts that are not in stock refuse the whole job card.
	if !c.refused(httpMethodPost, "Job Card", "", `{"reference":"JC-2","customer_vehicle":"`+vehicle+`","opened_on":"2026-12-01","parts":[{"product":"`+filter+`","quantity":1},{"product":"`+pads+`","quantity":3}]}`) {
		t.Fatal("a job card using more parts than are in stock was accepted")
	}
	if got, other := workshopStock(t, c, pads), workshopStock(t, c, filter); got != 2 || other != 9 {
		t.Fatalf("stock after a refused job card = pads %v, filter %v; want 2 and 9", got, other)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabJob Card` WHERE reference = 'JC-2'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused job card left %d record(s), %v", n, err)
	}

	// Known limit, kept visible here and in the README: the Engine does not check
	// constraints on child rows, so a negative part quantity is accepted and
	// puts stock back. If the Engine starts refusing it, update both.
	if c.refused(httpMethodPost, "Job Card", "", `{"reference":"JC-NEG","customer_vehicle":"`+vehicle+`","opened_on":"2026-12-01","parts":[{"product":"`+pads+`","quantity":-1}]}`) {
		t.Fatal("a negative part quantity is now refused: update the README limits")
	}
	if got := workshopStock(t, c, pads); got != 3 {
		t.Fatalf("pads stock after a negative part quantity = %v, want 3 (observed: stock goes up)", got)
	}

	// Rules on the job card itself.
	if !c.refused(httpMethodPost, "Job Card", "", `{"reference":"JC-3","customer_vehicle":"`+vehicle+`","opened_on":"2026-12-05","promised_on":"2026-12-01"}`) {
		t.Fatal("a promised date before the opened date was accepted")
	}
	if !c.refused(httpMethodPost, "Job Card", "", `{"reference":"JC-4","customer_vehicle":"`+vehicle+`","opened_on":"2026-12-01","labour_charge":-5}`) {
		t.Fatal("a negative labour charge was accepted")
	}
	if !c.refused(httpMethodPost, "Job Card", "", `{"reference":"JC-1","customer_vehicle":"`+vehicle+`","opened_on":"2026-12-01"}`) {
		t.Fatal("a second job card with the same reference was accepted")
	}
	if !c.refused(httpMethodPost, "Job Card", "", `{"reference":"JC-5","customer_vehicle":"`+vehicle+`"}`) {
		t.Fatal("a job card without an opened date was accepted")
	}

	// Editing a saved job card does not touch stock: effects apply on create only.
	if c.refused(httpMethodPut, "Job Card", card, `{"status":"In progress","parts":[{"product":"`+filter+`","quantity":5}]}`) {
		t.Fatal("editing the parts of an open job card was refused")
	}
	if got := workshopStock(t, c, filter); got != 9 {
		t.Fatalf("filter stock after editing the parts = %v, want 9 (edits do not move stock)", got)
	}
	var quantity float64
	if err := db.QueryRow("SELECT quantity FROM `tabJob Card__parts` WHERE parent = ? AND product = ?", card, filter).Scan(&quantity); err != nil || quantity != 5 {
		t.Fatalf("part quantity after the edit = %v, %v; want 5", quantity, err)
	}

	// Deleting a job card does not give the parts back either.
	gone := c.create("Job Card", `{"reference":"JC-DEL","customer_vehicle":"`+vehicle+`","opened_on":"2026-12-02","parts":[{"product":"`+filter+`","quantity":2}]}`)
	if got := workshopStock(t, c, filter); got != 7 {
		t.Fatalf("filter stock after a second job card = %v, want 7", got)
	}
	if c.refused(httpMethodDelete, "Job Card", gone, "") {
		t.Fatal("deleting an open job card was refused")
	}
	if got := workshopStock(t, c, filter); got != 7 {
		t.Fatalf("filter stock after deleting a job card = %v, want 7 (delete does not return parts)", got)
	}

	// A collected job card is frozen.
	if c.refused(httpMethodPut, "Job Card", card, `{"status":"Collected"}`) {
		t.Fatal("collecting a job card was refused")
	}
	other := c.create("Customer Vehicle", `{"registration":"KDB 456B"}`)
	if !c.refused(httpMethodPut, "Job Card", card, `{"customer_vehicle":"`+other+`"}`) {
		t.Fatal("the vehicle of a collected job card was changed")
	}
	if !c.refused(httpMethodPut, "Job Card", card, `{"opened_on":"2026-11-01"}`) {
		t.Fatal("the opened date of a collected job card was changed")
	}
	if got := c.value("Job Card", card, "customer_vehicle"); got != vehicle {
		t.Fatalf("vehicle after a refused change = %q, want %q", got, vehicle)
	}
	if got := c.value("Job Card", card, "opened_on"); !strings.HasPrefix(got, "2026-12-01") {
		t.Fatalf("opened date after a refused change = %q, want 2026-12-01", got)
	}
}
