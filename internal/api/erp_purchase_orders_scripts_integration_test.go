//go:build integration

package api

import (
	"database/sql"
	"strconv"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// Buying against orders with the packs as shipped: a receipt takes an order's lines, the
// order's status follows them, and a supplier bill is checked against what was received.
func TestPackScriptsPurchaseOrders(t *testing.T) {
	eachPackDialect(t, []string{erpSellingPack, erpPurchasingPack}, func(f *packFixture) {
		for _, currency := range []string{"KES", "USD"} {
			f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, ?)", currency, currency, currency, true)
		}
		supplier := f.create("Supplier", `{"supplier_name":"Dar Traders"}`)
		other := f.create("Supplier", `{"supplier_name":"Mombasa Paper"}`)
		flour := f.create("Product", `{"product_name":"Maize flour","selling_price":100,"stock_qty":0,"status":"Active"}`)
		rice := f.create("Product", `{"product_name":"Rice","selling_price":150,"stock_qty":0,"status":"Active"}`)

		order := func(ref, status, lines string) string {
			return f.create("Purchase Order", `{"reference":"`+ref+`","order_date":"2026-11-18","supplier":"`+supplier+`","status":"`+status+`","items":[`+lines+`]}`)
		}
		orderLine := func(product, quantity string) string {
			return `{"product":"` + product + `","quantity":` + quantity + `,"unit_cost":2}`
		}
		receiptLine := func(product, quantity string) string {
			return `{"product":"` + product + `","quantity":` + quantity + `,"unit_cost":2}`
		}
		receiptBody := func(ref, po, who, lines string) string {
			return `{"reference":"` + ref + `","received_on":"2026-11-20","supplier":"` + who + `","purchase_order":"` + po + `","items":[` + lines + `]}`
		}
		stock := func(product string) float64 { return f.amount("Product", product, "stock_qty") }

		// PO-7: 10 flour at 2 (total 20). Ordered, so receipts may take it.
		po7 := order("PO-7", "Draft", orderLine(flour, "10"))

		f.run("a receipt needs an order that exists", func(f *packFixture) {
			f.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-NOPE", "PO-NOPE", supplier, receiptLine(flour, "1")), "PO-NOPE")
		})
		f.run("a receipt must come from the order's supplier", func(f *packFixture) {
			f.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-WRONG", po7, other, receiptLine(flour, "1")), "another supplier")
		})
		f.run("a draft order takes no delivery until it is ordered", func(f *packFixture) {
			f.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-DRAFT", po7, supplier, receiptLine(flour, "1")), "PO-7 is Draft; mark it Ordered first.")
			f.update("Purchase Order", po7, `{"status":"Ordered"}`)
			f.expect("Purchase Order", po7, "status", "Ordered")
		})
		f.run("a product not on the order is refused", func(f *packFixture) {
			f.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-RICE", po7, supplier, receiptLine(rice, "1")), "is not on PO-7.")
		})
		f.run("a zero or negative quantity is refused", func(f *packFixture) {
			// The kernel refuses a zero or negative quantity before the script runs.
			f.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-ZERO", po7, supplier, receiptLine(flour, "0")), "quantity")
			f.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-NEG", po7, supplier, receiptLine(flour, "-2")), "quantity")
		})
		f.run("a partial receipt raises stock and the order becomes Partly Received", func(f *packFixture) {
			f.create("Goods Receipt", receiptBody("GR-1", po7, supplier, receiptLine(flour, "6")))
			if got := stock(flour); got != 6 {
				f.t.Fatalf("stock = %v, want 6", got)
			}
			f.expect("Purchase Order", po7, "status", "Partly Received")
			if lines := orderLines(f, po7); len(lines) != 1 || lines[0].received != 6 {
				f.t.Fatalf("lines = %+v, want one line with 6 received", lines)
			}
		})
		f.run("a receipt over what is still due is refused and moves nothing", func(f *packFixture) {
			f.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-OVER", po7, supplier, receiptLine(flour, "5")), "Only 4 of "+flour+" are still due on PO-7.")
			if got := stock(flour); got != 6 {
				f.t.Fatalf("stock after refused receipt = %v, want 6", got)
			}
			if rows := f.count("tabGoods Receipt", "reference = ?", "GR-OVER"); rows != 0 {
				f.t.Fatalf("refused receipt left %d rows", rows)
			}
			if lines := orderLines(f, po7); lines[0].received != 6 {
				f.t.Fatalf("received after refused receipt = %v, want 6", lines[0].received)
			}
		})
		f.run("the rest of the order completes it", func(f *packFixture) {
			f.create("Goods Receipt", receiptBody("GR-2", po7, supplier, receiptLine(flour, "4")))
			f.expect("Purchase Order", po7, "status", "Received")
			if got := stock(flour); got != 10 {
				f.t.Fatalf("stock = %v, want 10", got)
			}
		})
		f.run("a received order is final", func(f *packFixture) {
			f.refused(httpMethodPut, "Purchase Order", po7, `{"status":"Ordered"}`, "PO-7 is Received and cannot change status.")
			f.expect("Purchase Order", po7, "status", "Received")
		})

		// PO-8: two lines of flour (4 then 6) so that a receipt of 5 fills the first line and
		// then the second. Same-product lines count together.
		po8 := order("PO-8", "Ordered", orderLine(flour, "4")+","+orderLine(flour, "6"))
		f.run("a receipt fills the order's lines in line order", func(f *packFixture) {
			f.create("Goods Receipt", receiptBody("GR-8", po8, supplier, receiptLine(flour, "5")))
			lines := orderLines(f, po8)
			if len(lines) != 2 || lines[0].received != 4 || lines[1].received != 1 {
				f.t.Fatalf("lines = %+v, want received 4 then 1", lines)
			}
			f.expect("Purchase Order", po8, "status", "Partly Received")
		})

		po9 := order("PO-9", "Ordered", orderLine(flour, "4")+","+orderLine(flour, "6"))
		f.run("the due quantity of a product sums across its order lines", func(f *packFixture) {
			f.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-9", po9, supplier, receiptLine(flour, "11")), "Only 10 of "+flour+" are still due on PO-9.")
			f.create("Goods Receipt", receiptBody("GR-9A", po9, supplier, receiptLine(flour, "10")))
			f.expect("Purchase Order", po9, "status", "Received")
		})

		f.run("a person cannot set a receipt status by hand", func(f *packFixture) {
			po10 := order("PO-10", "Draft", orderLine(flour, "2"))
			f.refused(httpMethodPut, "Purchase Order", po10, `{"status":"Partly Received"}`, "An order is received through goods receipts.")
			f.refused(httpMethodPut, "Purchase Order", po10, `{"status":"Received"}`, "An order is received through goods receipts.")
			f.refused(httpMethodPost, "Purchase Order", "", `{"reference":"PO-10B","order_date":"2026-11-18","supplier":"`+supplier+`","status":"Received","items":[`+orderLine(flour, "1")+`]}`, "An order is received through goods receipts.")
		})
		f.run("a person moves an order between Draft, Ordered and Cancelled", func(f *packFixture) {
			po11 := order("PO-11", "Draft", orderLine(flour, "2"))
			f.update("Purchase Order", po11, `{"status":"Ordered"}`)
			f.update("Purchase Order", po11, `{"status":"Draft"}`)
			f.update("Purchase Order", po11, `{"status":"Ordered"}`)
			f.update("Purchase Order", po11, `{"status":"Cancelled"}`)
			f.expect("Purchase Order", po11, "status", "Cancelled")
			f.refused(httpMethodPut, "Purchase Order", po11, `{"status":"Ordered"}`, "PO-11 is Cancelled and cannot change status.")
		})
		f.run("an order with goods received cannot be cancelled or reopened", func(f *packFixture) {
			po12 := order("PO-12", "Ordered", orderLine(flour, "10"))
			f.create("Goods Receipt", receiptBody("GR-12", po12, supplier, receiptLine(flour, "3")))
			f.refused(httpMethodPut, "Purchase Order", po12, `{"status":"Cancelled"}`, "PO-12 cannot change from Partly Received to Cancelled.")
			f.refused(httpMethodPut, "Purchase Order", po12, `{"status":"Draft"}`, "PO-12 cannot change from Partly Received to Draft.")
			f.expect("Purchase Order", po12, "status", "Partly Received")
		})
		f.run("a product change on an order with goods received is refused", func(f *packFixture) {
			po13 := order("PO-13", "Ordered", orderLine(flour, "10"))
			f.create("Goods Receipt", receiptBody("GR-13", po13, supplier, receiptLine(flour, "5")))
			f.refused(httpMethodPut, "Purchase Order", po13, `{"items":[`+orderLine(rice, "10")+`]}`, "Items of PO-13 cannot change while goods are received on it.")
			if lines := orderLines(f, po13); len(lines) != 1 || lines[0].received != 5 {
				f.t.Fatalf("lines = %+v, want 5 received after the refused change", lines)
			}
		})

		f.run("a person's receipt moves stock by what it takes", func(f *packFixture) {
			before := stock(flour)
			po15 := order("PO-15", "Ordered", orderLine(flour, "5"))
			f.create("Goods Receipt", receiptBody("GR-15", po15, supplier, receiptLine(flour, "2")))
			if got := stock(flour) - before; got != 2 {
				f.t.Fatalf("stock moved by %v, want 2", got)
			}
			f.expect("Purchase Order", po15, "status", "Partly Received")
		})

		// Bills against receipts and orders. PO-14 is 10 flour at 2 (20), received as 6 then 4.
		po14 := order("PO-14", "Ordered", orderLine(flour, "10"))
		gr14a := f.create("Goods Receipt", receiptBody("GR-14A", po14, supplier, receiptLine(flour, "6")))
		gr14b := f.create("Goods Receipt", receiptBody("GR-14B", po14, supplier, receiptLine(flour, "4")))
		bill := func(ref, who, amount, currency, receipt, order string) string {
			body := `{"reference":"` + ref + `","supplier":"` + who + `","bill_date":"2026-11-21","amount":` + amount + `,"status":"Open"`
			if currency != "" {
				body += `,"currency":"` + currency + `"`
			}
			if receipt != "" {
				body += `,"goods_receipt":"` + receipt + `"`
			}
			if order != "" {
				body += `,"purchase_order":"` + order + `"`
			}
			return body + `}`
		}
		f.run("a bill from a receipt cannot exceed the receipt", func(f *packFixture) {
			f.refused(httpMethodPost, "Supplier Bill", "", bill("BILL-14X", supplier, "13", "", gr14a, ""), "more than "+gr14a+" of 12.")
		})
		f.run("a bill from a receipt must be from the receipt's supplier and currency", func(f *packFixture) {
			f.refused(httpMethodPost, "Supplier Bill", "", bill("BILL-14Y", other, "12", "", gr14a, ""), "from another supplier than "+gr14a)
			f.refused(httpMethodPost, "Supplier Bill", "", bill("BILL-14Z", supplier, "1", "USD", gr14a, ""), "in another currency than "+gr14a)
		})
		f.run("a bill naming an order and a receipt of another order is refused", func(f *packFixture) {
			po16 := order("PO-16", "Ordered", orderLine(rice, "1"))
			f.refused(httpMethodPost, "Supplier Bill", "", bill("BILL-16X", supplier, "1", "", gr14a, po16), "belongs to")
		})
		var billA string
		f.run("the bills of an order cannot exceed what its receipts total", func(f *packFixture) {
			billA = f.create("Supplier Bill", bill("BILL-14A", supplier, "12", "", gr14a, po14))
			billB := f.create("Supplier Bill", bill("BILL-14B", supplier, "8", "", gr14b, po14))
			f.refused(httpMethodPost, "Supplier Bill", "", bill("BILL-14C", supplier, "1", "", "", po14), "would total 21, more than the 20 received.")
			f.expect("Supplier Bill", billB, "status", "Open")
		})
		f.run("a cancelled bill stops counting against the order", func(f *packFixture) {
			f.update("Supplier Bill", billA, `{"status":"Cancelled"}`)
			billC := f.create("Supplier Bill", bill("BILL-14D", supplier, "1", "", "", po14))
			f.refused(httpMethodPut, "Supplier Bill", billC, `{"amount":30}`, "would total 38, more than the 20 received.")
		})
		f.run("a bill on an order with no receipts is refused", func(f *packFixture) {
			po17 := order("PO-17", "Ordered", orderLine(rice, "1"))
			f.refused(httpMethodPost, "Supplier Bill", "", bill("BILL-17", supplier, "1", "", "", po17), "would total 1, more than the 0 received.")
		})

		f.run("a buyer receives against an order and cannot set its status by hand", func(f *packFixture) {
			buyer := f.as("Buyer")
			before := stock(flour)
			po18 := order("PO-18", "Ordered", orderLine(flour, "5"))
			buyer.create("Goods Receipt", receiptBody("GR-18", po18, supplier, receiptLine(flour, "2")))
			if got := stock(flour) - before; got != 2 {
				f.t.Fatalf("stock moved by %v, want 2", got)
			}
			buyer.expect("Purchase Order", po18, "status", "Partly Received")
			buyer.refused(httpMethodPost, "Goods Receipt", "", receiptBody("GR-18B", po18, supplier, receiptLine(flour, "4")), "Only 3 of "+flour+" are still due on PO-18.")
			buyer.refused(httpMethodPut, "Purchase Order", po18, `{"status":"Received"}`, "An order is received through goods receipts.")
			buyer.expect("Purchase Order", po18, "status", "Partly Received")
		})
	})
}

// poLine is one line of an order as stored.
type poLine struct {
	product  string
	quantity float64
	received float64
}

// orderLines reads an order's lines in line order.
func orderLines(f *packFixture, po string) []poLine {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT product, quantity, received_qty FROM " + dl.QuoteIdent("tabPurchase Order__items") + " WHERE parent = ? ORDER BY idx"
	rows, err := f.db.Query(kdb.Rebind(dl, query), po)
	if err != nil {
		f.t.Fatalf("read order lines: %v", err)
	}
	defer rows.Close()
	var out []poLine
	for rows.Next() {
		var product, quantity, received sql.NullString
		if err := rows.Scan(&product, &quantity, &received); err != nil {
			f.t.Fatalf("scan order line: %v", err)
		}
		out = append(out, poLine{product: product.String, quantity: parseFloatOrZero(f, quantity.String), received: parseFloatOrZero(f, received.String)})
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate order lines: %v", err)
	}
	return out
}

func parseFloatOrZero(f *packFixture, text string) float64 {
	f.t.Helper()
	if text == "" {
		return 0
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		f.t.Fatalf("%q is not a number: %v", text, err)
	}
	return value
}
