//go:build integration

package api

import (
	"encoding/json"
	"testing"
	"time"
)

const assetsPack = "../../config/assets"

// assetsDay returns the date n days from today, in UTC, as the scripts read it.
func assetsDay(n int) string {
	return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02")
}

// assetsBody encodes a record's fields as a request body.
func assetsBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode %v: %v", fields, err)
	}
	return string(out)
}

// assetsCase runs one subtest on the fixture's dialect, with the fixture's roles.
func assetsCase(f *packFixture, name string, fn func(f *packFixture)) {
	f.t.Run(name, func(t *testing.T) {
		sub := *f
		sub.t = t
		fn(&sub)
	})
}

func TestPackScriptsAssets(t *testing.T) {
	eachPackDialect(t, []string{assetsPack}, func(f *packFixture) {
		assetsCase(f, "retired asset takes no assignment", func(f *packFixture) {
			retired := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "R-1", "asset_name": "Old van", "status": "Retired"}))
			f.refused(httpMethodPost, "Asset Assignment", "", assetsBody(f.t, map[string]any{
				"asset": retired, "assigned_to": "driver@example.com", "from_date": assetsDay(-5),
			}), "Old van is retired and cannot be assigned.")
		})

		assetsCase(f, "retired asset takes no maintenance task", func(f *packFixture) {
			retired := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "R-2", "asset_name": "Scrap cart", "status": "Retired"}))
			f.refused(httpMethodPost, "Maintenance Task", "", assetsBody(f.t, map[string]any{
				"asset": retired, "due_date": assetsDay(3),
			}), "Scrap cart is retired and takes no maintenance tasks.")
		})

		assetsCase(f, "overlapping dated assignments are refused, touching ones are not", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "O-1", "asset_name": "Delivery van"}))
			first := f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "driver1@example.com", "from_date": assetsDay(-30), "to_date": assetsDay(10),
			}))
			f.refused(httpMethodPost, "Asset Assignment", "", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "driver2@example.com", "from_date": assetsDay(0), "to_date": assetsDay(20),
			}), "Delivery van is assigned to driver1@example.com from "+assetsDay(-30)+".")
			f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "driver2@example.com", "from_date": assetsDay(11), "to_date": assetsDay(20),
			}))
			f.update("Asset Assignment", first, assetsBody(f.t, map[string]any{"note": "Kept after review"}))
		})

		assetsCase(f, "an open-ended assignment overlaps every later one", func(f *packFixture) {
			cart := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "O-2", "asset_name": "Hand cart"}))
			f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": cart, "assigned_to": "porter@example.com", "from_date": assetsDay(10),
			}))
			f.refused(httpMethodPost, "Asset Assignment", "", assetsBody(f.t, map[string]any{
				"asset": cart, "assigned_to": "other@example.com", "from_date": assetsDay(100), "to_date": assetsDay(110),
			}), "Hand cart is assigned to porter@example.com from "+assetsDay(10)+".")
			f.refused(httpMethodPost, "Asset Assignment", "", assetsBody(f.t, map[string]any{
				"asset": cart, "assigned_to": "other@example.com", "from_date": assetsDay(50),
			}), "Hand cart is assigned to porter@example.com")
			f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": cart, "assigned_to": "other@example.com", "from_date": assetsDay(3), "to_date": assetsDay(9),
			}))
		})

		assetsCase(f, "a current assignment puts an asset in store into use with its user", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "C-1", "asset_name": "Pickup", "status": "In store"}))
			f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "driver@example.com", "from_date": assetsDay(-2),
			}))
			f.expect("Asset", van, "status", "In use")
			f.expect("Asset", van, "assigned_to", "driver@example.com")
		})

		assetsCase(f, "a future assignment leaves the asset alone", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "C-2", "asset_name": "Lorry", "status": "In store"}))
			f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "driver@example.com", "from_date": assetsDay(7),
			}))
			f.expect("Asset", van, "status", "In store")
			f.expect("Asset", van, "assigned_to", "")
		})

		assetsCase(f, "an asset under repair stays under repair while assigned", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "C-3", "asset_name": "Truck", "status": "Under repair"}))
			f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "driver@example.com", "from_date": assetsDay(-2),
			}))
			f.expect("Asset", van, "status", "Under repair")
			f.expect("Asset", van, "assigned_to", "driver@example.com")
		})

		assetsCase(f, "ending an assignment clears the asset and returns it to store", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "E-1", "asset_name": "Estate car"}))
			current := f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "driver@example.com", "from_date": assetsDay(-30),
			}))
			f.expect("Asset", van, "status", "In use")
			f.update("Asset Assignment", current, assetsBody(f.t, map[string]any{"to_date": assetsDay(-1)}))
			f.expect("Asset", van, "assigned_to", "")
			f.expect("Asset", van, "status", "In store")
		})

		assetsCase(f, "ending an assignment leaves another holder and an asset under repair alone", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "E-2", "asset_name": "Minibus"}))
			f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "new@example.com", "from_date": assetsDay(-10),
			}))
			f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "old@example.com", "from_date": assetsDay(-60), "to_date": assetsDay(-40),
			}))
			f.expect("Asset", van, "assigned_to", "new@example.com")
			f.expect("Asset", van, "status", "In use")

			repair := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "E-3", "asset_name": "Tractor", "status": "Under repair"}))
			holder := f.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": repair, "assigned_to": "farm@example.com", "from_date": assetsDay(-10),
			}))
			f.update("Asset Assignment", holder, assetsBody(f.t, map[string]any{"to_date": assetsDay(-1)}))
			f.expect("Asset", repair, "assigned_to", "")
			f.expect("Asset", repair, "status", "Under repair")
		})

		assetsCase(f, "a planned task does not put the asset under repair", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "M-1", "asset_name": "Forklift"}))
			f.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(60)}))
			f.expect("Asset", van, "status", "In use")
		})

		assetsCase(f, "marking a repair Done returns the asset to service with its holder", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "M-2", "asset_name": "Ute", "status": "Under repair", "assigned_to": "driver@example.com"}))
			task := f.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(-3)}))
			f.update("Maintenance Task", task, assetsBody(f.t, map[string]any{"status": "Done", "done_on": assetsDay(0)}))
			f.expect("Asset", van, "status", "In use")
		})

		assetsCase(f, "marking a repair Done without a holder returns the asset to store", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "M-3", "asset_name": "Generator", "status": "Under repair"}))
			task := f.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(-1)}))
			f.update("Maintenance Task", task, assetsBody(f.t, map[string]any{"status": "Done", "done_on": assetsDay(0)}))
			f.expect("Asset", van, "status", "In store")
		})

		assetsCase(f, "a repair Done stays under repair while another task is due", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "M-4", "asset_name": "Compressor", "status": "Under repair"}))
			first := f.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(-5)}))
			second := f.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(0)}))
			f.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(30)}))

			f.update("Maintenance Task", first, assetsBody(f.t, map[string]any{"status": "Done", "done_on": assetsDay(0)}))
			f.expect("Asset", van, "status", "Under repair")
			f.update("Maintenance Task", second, assetsBody(f.t, map[string]any{"status": "Done", "done_on": assetsDay(0)}))
			f.expect("Asset", van, "status", "In store")
		})

		assetsCase(f, "a Done or Cancelled task cannot change status", func(f *packFixture) {
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "F-1", "asset_name": "Printer"}))
			done := f.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(-1)}))
			f.update("Maintenance Task", done, assetsBody(f.t, map[string]any{"status": "Done", "done_on": assetsDay(0)}))
			f.refused(httpMethodPut, "Maintenance Task", done, `{"status":"Planned"}`, "is Done and its status cannot change.")
			f.update("Maintenance Task", done, `{"notes":"Toner swapped"}`)

			cancelled := f.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(2)}))
			f.update("Maintenance Task", cancelled, `{"status":"Cancelled"}`)
			f.refused(httpMethodPut, "Maintenance Task", cancelled, `{"status":"Done","done_on":"`+assetsDay(0)+`"}`, "is Cancelled and its status cannot change.")
		})

		assetsCase(f, "asset user records assignment and repair; the asset follows; direct asset edit refused", func(f *packFixture) {
			user := f.as("Asset User")
			van := f.create("Asset", assetsBody(f.t, map[string]any{"asset_tag": "U-1", "asset_name": "Front van", "status": "Under repair"}))
			task := user.create("Maintenance Task", assetsBody(f.t, map[string]any{"asset": van, "due_date": assetsDay(-2)}))
			user.create("Asset Assignment", assetsBody(f.t, map[string]any{
				"asset": van, "assigned_to": "driver@example.com", "from_date": assetsDay(-1),
			}))
			f.expect("Asset", van, "assigned_to", "driver@example.com")
			f.expect("Asset", van, "status", "Under repair")

			user.update("Maintenance Task", task, assetsBody(f.t, map[string]any{"status": "Done", "done_on": assetsDay(0)}))
			f.expect("Asset", van, "status", "In use")

			if _, _, doc := user.send(httpMethodPut, "Asset", van, `{"notes":"Edited by hand"}`); doc != nil {
				t.Fatal("Asset User saved a direct edit of an asset")
			}
		})
	})
}
