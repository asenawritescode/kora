//go:build integration

package api

import (
	"strconv"
	"testing"
)

// The farm produce pack (and the selling pack it needs) as shipped, with its scripts run the way
// the Engine runs them. A farmer payment pays produce deliveries; a delivery's amount paid and
// status follow its payments, and a saved delivery stays fixed.
const farmProducePack = "../../config/_parked/farm-produce"

const (
	farmManagerRole = "Cooperative Manager"
	farmClerkRole   = "Produce Clerk"
)

func TestPackScriptsFarmProduce(t *testing.T) {
	eachPackDialect(t, []string{erpSellingPack, farmProducePack}, func(f *packFixture) {
		runFarmCase(f, "payments apply, set the delivery status and reverse on void", farmPaymentsApply)
		runFarmCase(f, "a delivery cannot be overpaid or paid twice", farmOverAllocation)
		runFarmCase(f, "a payment to another farmer's delivery is refused", farmWrongFarmer)
		runFarmCase(f, "create as Paid with one bad row changes no delivery", farmAtomicCreate)
		runFarmCase(f, "money beyond the deliveries is kept as farmer credit", farmCredit)
		runFarmCase(f, "a payment's status moves only Draft to Paid and Paid to Voided", farmStatusMoves)
		runFarmCase(f, "a saved delivery is frozen, its status is not set by a person, and it is never deleted", farmDeliveryFrozen)
		runFarmCase(f, "the front-line clerk records deliveries but cannot pay farmers", farmClerkRoles)
	})
}

// runFarmCase runs one case as a subtest of the dialect, with the fixture reporting to it.
func runFarmCase(f *packFixture, name string, body func(f *packFixture)) {
	f.t.Run(name, func(t *testing.T) {
		g := *f
		g.t = t
		body(&g)
	})
}

func farmNumber(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }

// farmProduct inserts a product with no stock and returns its name.
func farmProduct(f *packFixture, name string) string {
	f.exec("INSERT INTO "+f.quote("tabProduct")+" (name, reference, product_name, selling_price, stock_qty, status) VALUES (?, ?, ?, 60, 0, 'Active')", name, name, name)
	return name
}

// farmDelivery records a delivery of kg at price per kg and returns its name.
func farmDelivery(f *packFixture, farmer, product, reference string, kg, price float64) string {
	return f.create("Produce Delivery", `{"reference":"`+reference+`","farmer":"`+farmer+`","product":"`+product+`","delivery_date":"2026-12-03","quantity_kg":`+farmNumber(kg)+`,"grade":"A","price_per_kg":`+farmNumber(price)+`}`)
}

// farmExpect fails the test unless a money column of a record holds want.
func farmExpect(f *packFixture, recordType, name, column string, want float64) {
	f.t.Helper()
	if got := f.amount(recordType, name, column); got != want {
		f.t.Fatalf("%s %s.%s = %v, want %v", recordType, name, column, got, want)
	}
}

// farmAllocation is one row of a payment: amount paid on one delivery.
func farmAllocation(delivery string, amount string) string {
	return `{"produce_delivery":"` + delivery + `","amount":` + amount + `}`
}

// farmPaymentBody is a farmer payment as a request body with the given status and rows.
func farmPaymentBody(reference, farmer, amount, status string, rows ...string) string {
	body := `{"reference":"` + reference + `","payment_date":"2026-12-04","farmer":"` + farmer + `","amount":` + amount + `,"status":"` + status + `"`
	if len(rows) > 0 {
		body += `,"allocations":[`
		for i, row := range rows {
			if i > 0 {
				body += ","
			}
			body += row
		}
		body += "]"
	}
	return body + "}"
}

// farmRejected reports whether saving the record is refused, whatever the reason.
func farmRejected(f *packFixture, method resourceHTTPMethod, recordType, name, body string) bool {
	f.t.Helper()
	_, _, doc := f.send(method, recordType, name, body)
	return doc == nil
}

func farmPaymentsApply(f *packFixture) {
	m := f.as(farmManagerRole)
	farmer := m.create("Farmer", `{"member_no":"M-A","farmer_name":"Achieng Otieno"}`)
	product := farmProduct(f, "PROD-A")
	d1 := farmDelivery(m, farmer, product, "FD-A1", 100, 10.05)
	d2 := farmDelivery(m, farmer, product, "FD-A2", 3, 3.33)
	if got := f.amount("Produce Delivery", d1, "amount"); got != 1005 {
		f.t.Fatalf("delivery 1 amount = %v, want 1005", got)
	}
	if got := f.amount("Product", product, "stock_qty"); got != 103 {
		f.t.Fatalf("stock after deliveries = %v, want 103", got)
	}

	// A draft holds its rows without touching the deliveries; marking it Paid applies them.
	p1 := m.create("Farmer Payment", farmPaymentBody("FP-A1", farmer, "500.10", "Draft", farmAllocation(d1, "300.04")))
	if got := f.amount("Produce Delivery", d1, "amount_paid"); got != 0 {
		f.t.Fatalf("a draft moved the delivery's amount paid to %v", got)
	}
	m.update("Farmer Payment", p1, `{"status":"Paid"}`)
	f.expect("Produce Delivery", d1, "status", "Partly Paid")
	if got := f.amount("Produce Delivery", d1, "outstanding"); got != 704.96 {
		f.t.Fatalf("delivery 1 still owed = %v, want 704.96", got)
	}
	if got := f.amount("Farmer Payment", p1, "unallocated_amount"); got != 200.06 {
		f.t.Fatalf("payment 1 farmer credit = %v, want 200.06", got)
	}

	// Two payments add up exactly to the full price of the delivery.
	p2 := m.create("Farmer Payment", farmPaymentBody("FP-A2", farmer, "714.95", "Paid", farmAllocation(d1, "704.96"), farmAllocation(d2, "9.99")))
	farmExpect(f, "Produce Delivery", d1, "amount_paid", 1005)
	f.expect("Produce Delivery", d1, "status", "Paid")
	f.expect("Produce Delivery", d2, "status", "Paid")
	farmExpect(f, "Farmer Payment", p2, "unallocated_amount", 0)
	if got := f.amount("Product", product, "stock_qty"); got != 103 {
		f.t.Fatalf("payments moved stock to %v, want 103", got)
	}

	// A paid payment's amount is fixed, and it cannot be deleted; a draft's rows are not.
	m.refused(httpMethodPut, "Farmer Payment", p1, `{"amount":600}`, "cannot change. Void it instead")
	f.refused(httpMethodDelete, "Farmer Payment", p1, "", "void it instead of deleting it")
	draft := m.create("Farmer Payment", farmPaymentBody("FP-A3", farmer, "1", "Draft"))
	if _, _, doc := f.send(httpMethodDelete, "Farmer Payment", draft, ""); doc == nil {
		f.t.Fatal("a draft payment could not be deleted")
	}

	// Voiding reverses exactly what each payment applied, and a voided payment stays voided.
	m.update("Farmer Payment", p1, `{"status":"Voided"}`)
	if got := f.amount("Produce Delivery", d1, "amount_paid"); got != 704.96 {
		f.t.Fatalf("after voiding payment 1, delivery 1 paid = %v, want 704.96", got)
	}
	f.expect("Produce Delivery", d1, "status", "Partly Paid")
	m.update("Farmer Payment", p2, `{"status":"Voided"}`)
	farmExpect(f, "Produce Delivery", d1, "amount_paid", 0)
	f.expect("Produce Delivery", d1, "status", "Received")
	f.expect("Produce Delivery", d2, "status", "Received")
	m.refused(httpMethodPut, "Farmer Payment", p1, `{"status":"Paid"}`, "cannot change from Voided to Paid")
}

func farmOverAllocation(f *packFixture) {
	m := f.as(farmManagerRole)
	farmer := m.create("Farmer", `{"member_no":"M-B","farmer_name":"Juma Kamau"}`)
	product := farmProduct(f, "PROD-B")
	d1 := farmDelivery(m, farmer, product, "FD-B1", 10, 5)
	d2 := farmDelivery(m, farmer, product, "FD-B2", 10, 5)

	over := m.create("Farmer Payment", farmPaymentBody("FP-B1", farmer, "60", "Draft", farmAllocation(d1, "50.01")))
	m.refused(httpMethodPut, "Farmer Payment", over, `{"status":"Paid"}`, "has only 50 still owed")

	total := m.create("Farmer Payment", farmPaymentBody("FP-B2", farmer, "10", "Draft", farmAllocation(d1, "6"), farmAllocation(d2, "6")))
	m.refused(httpMethodPut, "Farmer Payment", total, `{"status":"Paid"}`, "more than the payment of 10")

	zero := m.create("Farmer Payment", farmPaymentBody("FP-B3", farmer, "10", "Draft", farmAllocation(d1, "0")))
	m.refused(httpMethodPut, "Farmer Payment", zero, `{"status":"Paid"}`, "must be more than zero")
	farmExpect(f, "Produce Delivery", d1, "amount_paid", 0)
}

func farmWrongFarmer(f *packFixture) {
	m := f.as(farmManagerRole)
	owner := m.create("Farmer", `{"member_no":"M-C1","farmer_name":"Wanjiku Njeri"}`)
	other := m.create("Farmer", `{"member_no":"M-C2","farmer_name":"Peter Mwangi"}`)
	product := farmProduct(f, "PROD-C")
	delivery := farmDelivery(m, owner, product, "FD-C1", 5, 4)

	payment := m.create("Farmer Payment", farmPaymentBody("FP-C1", other, "20", "Draft", farmAllocation(delivery, "20")))
	m.refused(httpMethodPut, "Farmer Payment", payment, `{"status":"Paid"}`, "belongs to another farmer")
	farmExpect(f, "Produce Delivery", delivery, "amount_paid", 0)
}

func farmAtomicCreate(f *packFixture) {
	m := f.as(farmManagerRole)
	owner := m.create("Farmer", `{"member_no":"M-D1","farmer_name":"Grace Akinyi"}`)
	other := m.create("Farmer", `{"member_no":"M-D2","farmer_name":"Samuel Kiptoo"}`)
	product := farmProduct(f, "PROD-D")
	good := farmDelivery(m, owner, product, "FD-D1", 5, 4)
	bad := farmDelivery(m, other, product, "FD-D2", 5, 4)

	m.refused(httpMethodPost, "Farmer Payment", "", farmPaymentBody("FP-D1", owner, "30", "Paid",
		farmAllocation(good, "10"), farmAllocation(bad, "5")), "belongs to another farmer")
	farmExpect(f, "Produce Delivery", good, "amount_paid", 0)
	f.expect("Produce Delivery", good, "status", "Received")
	if n := f.count("tabFarmer Payment", "reference = ?", "FP-D1"); n != 0 {
		f.t.Fatalf("a refused payment was stored %d time(s)", n)
	}
}

func farmCredit(f *packFixture) {
	m := f.as(farmManagerRole)
	farmer := m.create("Farmer", `{"member_no":"M-E","farmer_name":"Asha Hassan"}`)
	product := farmProduct(f, "PROD-E")
	delivery := farmDelivery(m, farmer, product, "FD-E1", 10, 10)

	payment := m.create("Farmer Payment", farmPaymentBody("FP-E1", farmer, "100", "Paid", farmAllocation(delivery, "40")))
	farmExpect(f, "Farmer Payment", payment, "unallocated_amount", 60)
	farmExpect(f, "Produce Delivery", delivery, "outstanding", 60)
	f.expect("Produce Delivery", delivery, "status", "Partly Paid")
}

func farmStatusMoves(f *packFixture) {
	m := f.as(farmManagerRole)
	farmer := m.create("Farmer", `{"member_no":"M-F","farmer_name":"Lydia Chebet"}`)

	draft := m.create("Farmer Payment", farmPaymentBody("FP-F1", farmer, "6", "Draft"))
	m.refused(httpMethodPut, "Farmer Payment", draft, `{"status":"Voided"}`, "cannot change from Draft to Voided")
	m.refused(httpMethodPost, "Farmer Payment", "", farmPaymentBody("FP-F2", farmer, "6", "Voided"), "recorded as Draft or Paid, not Voided")
}

func farmDeliveryFrozen(f *packFixture) {
	m := f.as(farmManagerRole)
	farmer := m.create("Farmer", `{"member_no":"M-G","farmer_name":"Hassan Yusuf"}`)
	product := farmProduct(f, "PROD-G")
	delivery := farmDelivery(m, farmer, product, "FD-G1", 8, 5)

	// A person cannot set the status: the write is accepted but the status is not changed.
	m.update("Produce Delivery", delivery, `{"status":"Paid"}`)
	f.expect("Produce Delivery", delivery, "status", "Received")
	m.refused(httpMethodPut, "Produce Delivery", delivery, `{"quantity_kg":1}`, "cannot be changed when status is Received")
	m.refused(httpMethodPut, "Produce Delivery", delivery, `{"price_per_kg":9}`, "cannot be changed when status is Received")
	if got := f.amount("Product", product, "stock_qty"); got != 8 {
		f.t.Fatalf("stock after refused edits = %v, want 8", got)
	}
	// Nobody deletes a delivery, the administrator included: stock would be left wrong.
	f.refused(httpMethodDelete, "Produce Delivery", delivery, "", "cannot be deleted")
}

func farmClerkRoles(f *packFixture) {
	clerk := f.as(farmClerkRole)
	m := f.as(farmManagerRole)
	farmer := m.create("Farmer", `{"member_no":"M-H","farmer_name":"Mary Atieno"}`)
	product := farmProduct(f, "PROD-H")

	delivery := farmDelivery(clerk, farmer, product, "FD-H1", 4, 2)
	if got := f.amount("Product", product, "stock_qty"); got != 4 {
		f.t.Fatalf("stock after the clerk's delivery = %v, want 4", got)
	}
	if !farmRejected(clerk, httpMethodPost, "Farmer Payment", "", farmPaymentBody("FP-H1", farmer, "2", "Draft")) {
		f.t.Fatal("the clerk recorded a farmer payment")
	}
	if !farmRejected(clerk, httpMethodPut, "Produce Delivery", delivery, `{"quantity_kg":1}`) {
		f.t.Fatal("the clerk edited a delivery")
	}
}
