//go:build integration

package api

import (
	"strings"
	"testing"
)

// The job-cards pack with its scripts, on MySQL and PostgreSQL. A job card's parts move the
// stock of their products: a new card takes them off through its quantity effect, a later
// change moves the difference, and a delete gives them back. The Mechanic reads and writes
// job cards but cannot read Product; the Cashier role from the selling pack reads it.
const jobCardsPack = "../../config/_parked/job-cards"

func jobCardPart(product, quantity string) string {
	return `{"product":"` + product + `","quantity":` + quantity + `}`
}

func jobCardParts(rows ...string) string {
	return "[" + strings.Join(rows, ",") + "]"
}

// inCase returns the fixture acting inside one subtest, so a failure lands on that subtest.
func (f *packFixture) inCase(t *testing.T) *packFixture {
	other := *f
	other.t = t
	return &other
}

// deleted deletes a record as the fixture's role and fails the test if it is refused.
func (f *packFixture) deleted(recordType, name string) {
	f.t.Helper()
	if _, reply, doc := f.send(httpMethodDelete, recordType, name, ""); doc == nil {
		f.t.Fatalf("delete %s %s refused: %s", recordType, name, reply)
	}
}

func TestPackScriptsJobCards(t *testing.T) {
	eachPackDialect(t, []string{erpSellingPack, jobCardsPack}, func(f *packFixture) {
		filter := f.create("Product", `{"reference":"W-FILTER","product_name":"Oil filter","selling_price":500,"stock_qty":10}`)
		pads := f.create("Product", `{"reference":"W-PADS","product_name":"Brake pads","selling_price":2500,"stock_qty":4}`)
		vehicle := f.create("Customer Vehicle", `{"registration":"KDA 123A","make":"Toyota","model":"Probox"}`)
		var card, spare string

		// run executes one case as the Mechanic who also holds the selling reader role.
		run := func(name string, fn func(g, mechanic *packFixture)) {
			f.t.Run(name, func(tt *testing.T) {
				g := f.inCase(tt)
				fn(g, g.as("Mechanic", "Cashier"))
			})
		}
		stockIs := func(g *packFixture, wantFilter, wantPads float64) {
			g.t.Helper()
			if got := g.amount("Product", filter, "stock_qty"); got != wantFilter {
				g.t.Fatalf("oil filter stock = %v, want %v", got, wantFilter)
			}
			if got := g.amount("Product", pads, "stock_qty"); got != wantPads {
				g.t.Fatalf("brake pads stock = %v, want %v", got, wantPads)
			}
		}
		cardBody := func(reference, status string, parts ...string) string {
			body := `{"reference":"` + reference + `","customer_vehicle":"` + vehicle + `","opened_on":"2026-12-01","labour_charge":1500,"parts":` + jobCardParts(parts...)
			if status != "" {
				body += `,"status":"` + status + `"`
			}
			return body + `}`
		}

		run("a new card refuses a part quantity of zero or less and a status other than Booked or In progress", func(g, m *packFixture) {
			m.refused(httpMethodPost, "Job Card", "", cardBody("JC-ZERO", "", jobCardPart(filter, "0")), "needs a quantity above zero")
			m.refused(httpMethodPost, "Job Card", "", cardBody("JC-NEG", "", jobCardPart(filter, "-1")), "needs a quantity above zero")
			m.refused(httpMethodPost, "Job Card", "", cardBody("JC-DONE", "Done", jobCardPart(filter, "1")), "starts as Booked or In progress")
			m.refused(httpMethodPost, "Job Card", "", cardBody("JC-SHORT", "", jobCardPart(filter, "11")), "insufficient quantity")
			stockIs(g, 10, 4)
			if n := g.count("tabJob Card", "customer_vehicle = ?", vehicle); n != 0 {
				g.t.Fatalf("refused cards left %d record(s)", n)
			}
		})

		run("creating a card takes its parts off stock once", func(g, m *packFixture) {
			card = m.create("Job Card", cardBody("JC-1", "", jobCardPart(filter, "1"), jobCardPart(pads, "2")))
			g.expect("Job Card", card, "status", "Booked")
			stockIs(g, 9, 2)
		})

		run("status moves follow the workflow and Collected is final", func(g, m *packFixture) {
			m.refused(httpMethodPut, "Job Card", card, `{"status":"Done"}`, "cannot move from Booked to Done. Allowed: In progress.")
			m.update("Job Card", card, `{"status":"In progress"}`)
			m.update("Job Card", card, `{"status":"Waiting for parts"}`)
			m.refused(httpMethodPut, "Job Card", card, `{"status":"Done"}`, "cannot move from Waiting for parts to Done")
			m.update("Job Card", card, `{"status":"In progress"}`)
			m.update("Job Card", card, `{"status":"Done"}`)
			m.update("Job Card", card, `{"status":"In progress"}`)
			m.update("Job Card", card, `{"status":"Done"}`)
			g.expect("Job Card", card, "status", "Done")
		})

		run("a parts change moves stock by the difference per product", func(g, m *packFixture) {
			m.update("Job Card", card, `{"parts":`+jobCardParts(jobCardPart(filter, "4"))+`}`)
			stockIs(g, 6, 4)
			m.refused(httpMethodPut, "Job Card", card, `{"parts":`+jobCardParts(jobCardPart(filter, "11"))+`}`, "Only 6 of")
			stockIs(g, 6, 4)
			if n := g.count("tabJob Card__parts", "parent = ? AND quantity = ?", card, 4); n != 1 {
				g.t.Fatalf("refused change left %d filter rows of 4, want 1", n)
			}
			m.refused(httpMethodPut, "Job Card", card, `{"parts":`+jobCardParts(jobCardPart(filter, "0"))+`}`, "needs a quantity above zero")
			m.update("Job Card", card, `{"parts":`+jobCardParts(jobCardPart(filter, "4"), jobCardPart(pads, "1"))+`}`)
			stockIs(g, 6, 3)
		})

		run("deleting a card gives its parts back, and a mechanic cannot delete", func(g, m *packFixture) {
			spare = g.create("Job Card", cardBody("JC-2", "In progress", jobCardPart(filter, "2"), jobCardPart(pads, "1")))
			stockIs(g, 4, 2)
			m.refused(httpMethodDelete, "Job Card", spare, "", "")
			g.deleted("Job Card", spare)
			stockIs(g, 6, 3)
			if n := g.count("tabJob Card", "name = ?", spare); n != 0 {
				g.t.Fatalf("deleted card still stored")
			}
		})

		run("a collected card cannot change status, parts, labour or be deleted", func(g, m *packFixture) {
			m.update("Job Card", card, `{"status":"Collected"}`)
			m.refused(httpMethodPut, "Job Card", card, `{"status":"In progress"}`, "is collected and cannot change status")
			m.refused(httpMethodPut, "Job Card", card, `{"parts":`+jobCardParts(jobCardPart(filter, "1"))+`}`, "its parts cannot change")
			m.refused(httpMethodPut, "Job Card", card, `{"labour_charge":2000}`, "labour charge cannot change")
			m.update("Job Card", card, `{"notes":"Collected by owner"}`)
			g.refused(httpMethodDelete, "Job Card", card, "", "is collected and cannot be deleted")
			stockIs(g, 6, 3)
		})

		run("a mechanic cannot edit Product stock directly", func(g, m *packFixture) {
			m.refused(httpMethodPut, "Product", filter, `{"stock_qty":100}`, "not permitted")
			m.refused(httpMethodDelete, "Job Card", card, "", "not permitted")
			stockIs(g, 6, 3)
		})

		run("a mechanic without the selling reader role cannot change a saved card's parts", func(g, m *packFixture) {
			open := g.create("Job Card", cardBody("JC-3", "", jobCardPart(filter, "1")))
			stockIs(g, 5, 3)
			alone := g.as("Mechanic")
			alone.refused(httpMethodPut, "Job Card", open, `{"parts":`+jobCardParts(jobCardPart(filter, "2"))+`}`, "Cannot check product")
			stockIs(g, 5, 3)
			g.deleted("Job Card", open)
			stockIs(g, 6, 3)
		})
	})
}
