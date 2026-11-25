//go:build integration

package api

import (
	"testing"
)

// The parked batch-expiry pack with its scripts, loaded with the selling pack that
// provides Product. Dispensing takes quantities from batches; the front-line role
// (Dispensing Assistant) may only read batches and still dispenses through the script.
const batchExpiryPack = "../../config/_parked/batch-expiry"

// rxLine is one prescription line; batch may be empty.
func rxLine(product, batch string, quantity float64) map[string]any {
	line := map[string]any{"product": product, "quantity": quantity}
	if batch != "" {
		line["batch"] = batch
	}
	return line
}

// expectQuantity checks a batch's number column against want, compared as a number.
func expectQuantity(f *packFixture, batch, column string, want float64) {
	f.t.Helper()
	if got := f.amount("Medicine Batch", batch, column); got != want {
		f.t.Fatalf("Medicine Batch %s.%s = %v, want %v", batch, column, got, want)
	}
}

func TestPackScriptsBatchExpiry(t *testing.T) {
	eachPackDialect(t, []string{erpSellingPack, batchExpiryPack}, func(f *packFixture) {
		medicine := f.create("Product", `{"product_name":"Amoxicillin 500mg","selling_price":300,"stock_qty":0,"status":"Active"}`)
		otherMedicine := f.create("Product", `{"product_name":"Paracetamol 500mg","selling_price":50,"stock_qty":0,"status":"Active"}`)
		patient := f.create("Patient", `{"patient_name":"Achieng Otieno"}`)

		batch := func(f *packFixture, no, product, expiry string, received float64) string {
			return f.create("Medicine Batch", jsonBody(t, map[string]any{
				"batch_no": no, "product": product, "expiry_date": expiry, "quantity_received": received,
			}))
		}
		prescribe := func(f *packFixture, ref, status string, lines ...map[string]any) string {
			return f.create("Prescription", jsonBody(t, map[string]any{
				"reference": ref, "patient": patient, "status": status, "lines": lines,
			}))
		}
		setStatus := func(f *packFixture, rx, status string) {
			f.update("Prescription", rx, jsonBody(t, map[string]any{"status": status}))
		}

		f.run("a batch starts with quantity on hand equal to what was received", func(f *packFixture) {
			full := batch(f, "B-FULL", medicine, "2030-01-01", 10)
			empty := batch(f, "B-EMPTY", medicine, "2030-01-01", 0)
			expectQuantity(f, full, "quantity_on_hand", 10)
			expectQuantity(f, empty, "quantity_on_hand", 0)
		})

		f.run("changing the quantity received moves quantity on hand by the same difference", func(f *packFixture) {
			b := batch(f, "B-RECV", medicine, "2030-01-01", 10)
			f.update("Medicine Batch", b, `{"quantity_received":15}`)
			expectQuantity(f, b, "quantity_on_hand", 15)
			f.update("Medicine Batch", b, `{"quantity_received":12}`)
			expectQuantity(f, b, "quantity_on_hand", 12)
		})

		f.run("quantity on hand cannot be typed in directly", func(f *packFixture) {
			b := batch(f, "B-TYPED", medicine, "2030-01-01", 10)
			f.update("Medicine Batch", b, `{"quantity_on_hand":99}`)
			expectQuantity(f, b, "quantity_on_hand", 10)
		})

		f.run("a receipt cannot take a batch below what prescriptions dispensed", func(f *packFixture) {
			b := batch(f, "B-TAKEN", medicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-TAKEN", "Received", rxLine(medicine, b, 4))
			setStatus(f, rx, "Dispensed")
			expectQuantity(f, b, "quantity_on_hand", 6)
			f.refused(httpMethodPut, "Medicine Batch", b, `{"quantity_received":3}`, "B-TAKEN would have -1 on hand")
			expectQuantity(f, b, "quantity_on_hand", 6)
			expectQuantity(f, b, "quantity_received", 10)
		})

		f.run("dispensing takes each line's quantity from its batch", func(f *packFixture) {
			b := batch(f, "B-DISP", medicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-DISP", "Received", rxLine(medicine, b, 4))
			expectQuantity(f, b, "quantity_on_hand", 10)
			setStatus(f, rx, "Dispensed")
			f.expect("Prescription", rx, "status", "Dispensed")
			expectQuantity(f, b, "quantity_on_hand", 6)
		})

		f.run("a prescription created as dispensed takes its batch quantity", func(f *packFixture) {
			b := batch(f, "B-CREATE", medicine, "2030-01-01", 5)
			prescribe(f, "RX-CREATE", "Dispensed", rxLine(medicine, b, 2))
			expectQuantity(f, b, "quantity_on_hand", 3)
		})

		f.run("two lines on one batch are summed", func(f *packFixture) {
			b := batch(f, "B-SUM", medicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-SUM", "Received", rxLine(medicine, b, 3), rxLine(medicine, b, 4))
			setStatus(f, rx, "Dispensed")
			expectQuantity(f, b, "quantity_on_hand", 3)
		})

		f.run("an expired batch refuses the dispense and changes no batch", func(f *packFixture) {
			good := batch(f, "B-GOOD", medicine, "2030-01-01", 10)
			old := batch(f, "B-OLD", medicine, "2020-01-01", 10)
			rx := prescribe(f, "RX-OLD", "Received", rxLine(medicine, good, 1), rxLine(medicine, old, 1))
			f.refused(httpMethodPut, "Prescription", rx, `{"status":"Dispensed"}`, "Batch B-OLD expired on 2020-01-01.")
			f.expect("Prescription", rx, "status", "Received")
			expectQuantity(f, good, "quantity_on_hand", 10)
		})

		f.run("a short batch refuses the dispense with nothing taken", func(f *packFixture) {
			good := batch(f, "B-OK", medicine, "2030-01-01", 10)
			short := batch(f, "B-SHORT", medicine, "2030-01-01", 2)
			rx := prescribe(f, "RX-SHORT", "Received", rxLine(medicine, good, 1), rxLine(medicine, short, 3))
			f.refused(httpMethodPut, "Prescription", rx, `{"status":"Dispensed"}`, "Batch B-SHORT has only 2 on hand, not 3.")
			expectQuantity(f, good, "quantity_on_hand", 10)
			expectQuantity(f, short, "quantity_on_hand", 2)
		})

		f.run("a batch of another medicine is refused", func(f *packFixture) {
			wrong := batch(f, "B-WRONG", otherMedicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-WRONG", "Received", rxLine(medicine, wrong, 1))
			f.refused(httpMethodPut, "Prescription", rx, `{"status":"Dispensed"}`, "Batch B-WRONG is not for")
			expectQuantity(f, wrong, "quantity_on_hand", 10)
		})

		f.run("a line without a batch cannot be dispensed", func(f *packFixture) {
			rx := prescribe(f, "RX-NOBATCH", "Received", rxLine(medicine, "", 1))
			f.refused(httpMethodPut, "Prescription", rx, `{"status":"Dispensed"}`, "needs a batch to dispense")
		})

		f.run("a line quantity must be above zero", func(f *packFixture) {
			f.refused(httpMethodPost, "Prescription", "", jsonBody(t, map[string]any{
				"reference": "RX-ZERO", "patient": patient, "lines": []map[string]any{rxLine(medicine, "", 0)},
			}), "needs a quantity above zero")
		})

		f.run("cancelling a dispensed prescription returns every line to its batch", func(f *packFixture) {
			b := batch(f, "B-RET", medicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-RET", "Received", rxLine(medicine, b, 2), rxLine(medicine, b, 3))
			setStatus(f, rx, "Dispensed")
			expectQuantity(f, b, "quantity_on_hand", 5)
			setStatus(f, rx, "Cancelled")
			f.expect("Prescription", rx, "status", "Cancelled")
			expectQuantity(f, b, "quantity_on_hand", 10)
		})

		f.run("cancelling a waiting prescription changes no batch", func(f *packFixture) {
			b := batch(f, "B-WAIT", medicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-WAIT", "Received", rxLine(medicine, b, 1))
			setStatus(f, rx, "Cancelled")
			expectQuantity(f, b, "quantity_on_hand", 10)
		})

		f.run("a dispensed prescription's medicines are frozen", func(f *packFixture) {
			b := batch(f, "B-FROZEN", medicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-FROZEN", "Received", rxLine(medicine, b, 2))
			setStatus(f, rx, "Dispensed")
			f.refused(httpMethodPut, "Prescription", rx, jsonBody(t, map[string]any{
				"lines": []map[string]any{rxLine(medicine, b, 5)},
			}), "is dispensed; its medicines cannot change")
			expectQuantity(f, b, "quantity_on_hand", 8)
		})

		f.run("a dispensed prescription cannot go back to received", func(f *packFixture) {
			b := batch(f, "B-BACK", medicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-BACK", "Received", rxLine(medicine, b, 2))
			setStatus(f, rx, "Dispensed")
			f.refused(httpMethodPut, "Prescription", rx, `{"status":"Received"}`, "cannot go back to Received")
		})

		f.run("a cancelled prescription's status is final", func(f *packFixture) {
			b := batch(f, "B-FINAL", medicine, "2030-01-01", 10)
			rx := prescribe(f, "RX-FINAL", "Received", rxLine(medicine, b, 2))
			setStatus(f, rx, "Cancelled")
			f.refused(httpMethodPut, "Prescription", rx, `{"status":"Dispensed"}`, "its status cannot change")
		})

		f.run("the dispensing assistant dispenses a batch it may only read", func(f *packFixture) {
			assistant := f.as("Dispensing Assistant", "Cashier")
			b := batch(f, "B-FRONT", medicine, "2030-01-01", 10)
			rx := assistant.create("Prescription", jsonBody(t, map[string]any{
				"reference": "RX-FRONT", "patient": patient, "lines": []map[string]any{rxLine(medicine, b, 4)},
			}))
			assistant.update("Prescription", rx, `{"status":"Dispensed"}`)
			expectQuantity(f, b, "quantity_on_hand", 6)
			assistant.update("Prescription", rx, `{"status":"Cancelled"}`)
			expectQuantity(f, b, "quantity_on_hand", 10)
			if _, _, doc := assistant.send(httpMethodPut, "Medicine Batch", b, `{"quantity_received":50}`); doc != nil {
				t.Fatal("the dispensing assistant edited a medicine batch directly")
			}
			expectQuantity(f, b, "quantity_received", 10)
		})
	})
}
