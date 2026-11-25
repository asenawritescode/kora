//go:build integration

package api

import "testing"

// The menu-tables pack as shipped, with its Kitchen Order script run the way the Engine runs
// it: a new order takes its table, every new or changed line needs a quantity and an available
// item (an order needs at least one line: the Engine's required check refuses an empty one first). A Waiter may only read tables, yet taking an order
// updates the table.
const (
	menuTablesScriptsPack    = "../../config/_parked/menu-tables"
	menuTablesScriptsSelling = "../../config/_parked/erpsell"
)

func TestPackScriptsMenuTables(t *testing.T) {
	eachPackDialect(t, []string{menuTablesScriptsSelling, menuTablesScriptsPack}, func(f *packFixture) {
		runPackCase(f, "a new order makes a free table occupied", func(f *packFixture) {
			g := f.as("Restaurant Manager")
			item := g.create("Menu Item", `{"item_name":"Beef stew","category":"Main","price":650}`)
			table := g.create("Dining Table", `{"table_no":"A1","seats":4}`)
			g.create("Kitchen Order", `{"reference":"KO-A1","dining_table":"`+table+`","lines":[{"menu_item":"`+item+`","quantity":1}]}`)
			g.expect("Dining Table", table, "status", "Occupied")
		})

		runPackCase(f, "a reserved table is taken by a new order", func(f *packFixture) {
			g := f.as("Restaurant Manager")
			item := g.create("Menu Item", `{"item_name":"Masala tea","category":"Drink","price":120}`)
			table := g.create("Dining Table", `{"table_no":"A2","seats":2,"status":"Reserved"}`)
			g.create("Kitchen Order", `{"reference":"KO-A2","dining_table":"`+table+`","lines":[{"menu_item":"`+item+`","quantity":2}]}`)
			g.expect("Dining Table", table, "status", "Occupied")
		})

		runPackCase(f, "a line quantity below one is refused", func(f *packFixture) {
			g := f.as("Restaurant Manager")
			item := g.create("Menu Item", `{"item_name":"Chapati","category":"Side","price":40}`)
			table := g.create("Dining Table", `{"table_no":"A3","seats":2}`)
			g.refused(httpMethodPost, "Kitchen Order", "", `{"reference":"KO-A3","dining_table":"`+table+`","lines":[{"menu_item":"`+item+`","quantity":0}]}`,
				"Quantity must be at least 1 for Chapati.")
			g.expect("Dining Table", table, "status", "Free")
		})

		runPackCase(f, "an unavailable item is refused on a new order", func(f *packFixture) {
			g := f.as("Restaurant Manager")
			item := g.create("Menu Item", `{"item_name":"Fish of the day","category":"Main","price":900}`)
			g.update("Menu Item", item, `{"available":0}`)
			table := g.create("Dining Table", `{"table_no":"A4","seats":2}`)
			g.refused(httpMethodPost, "Kitchen Order", "", `{"reference":"KO-A4","dining_table":"`+table+`","lines":[{"menu_item":"`+item+`","quantity":1}]}`,
				"Fish of the day is not available.")
			g.expect("Dining Table", table, "status", "Free")
		})

		runPackCase(f, "an order needs at least one line", func(f *packFixture) {
			g := f.as("Restaurant Manager")
			item := g.create("Menu Item", `{"item_name":"Samosa","category":"Starter","price":80}`)
			table := g.create("Dining Table", `{"table_no":"A5","seats":2}`)
			g.refused(httpMethodPost, "Kitchen Order", "", `{"reference":"KO-A5","dining_table":"`+table+`","lines":[]}`,
				"Items is required.")
			order := g.create("Kitchen Order", `{"reference":"KO-A6","dining_table":"`+table+`","lines":[{"menu_item":"`+item+`","quantity":1}]}`)
			g.refused(httpMethodPut, "Kitchen Order", order, `{"lines":[]}`, "Items is required.")
		})

		runPackCase(f, "an old order stays editable after its item is marked unavailable", func(f *packFixture) {
			g := f.as("Restaurant Manager")
			item := g.create("Menu Item", `{"item_name":"Mandazi","category":"Starter","price":30}`)
			table := g.create("Dining Table", `{"table_no":"A7","seats":2}`)
			order := g.create("Kitchen Order", `{"reference":"KO-A7","dining_table":"`+table+`","lines":[{"menu_item":"`+item+`","quantity":1}]}`)
			g.update("Menu Item", item, `{"available":0}`)
			g.update("Kitchen Order", order, `{"notes":"Extra napkins"}`)
			g.expect("Kitchen Order", order, "notes", "Extra napkins")
			g.refused(httpMethodPut, "Kitchen Order", order, `{"lines":[{"menu_item":"`+item+`","quantity":1},{"menu_item":"`+item+`","quantity":2}]}`,
				"Mandazi is not available.")
		})

		runPackCase(f, "a waiter takes an order, which occupies the table", func(f *packFixture) {
			w := f.as("Waiter")
			m := f.as("Restaurant Manager")
			item := m.create("Menu Item", `{"item_name":"Juice","category":"Drink","price":200}`)
			table := m.create("Dining Table", `{"table_no":"W1","seats":4}`)
			order := w.create("Kitchen Order", `{"reference":"KO-W1","dining_table":"`+table+`","lines":[{"menu_item":"`+item+`","quantity":2}]}`)
			m.expect("Dining Table", table, "status", "Occupied")

			w.refused(httpMethodPut, "Dining Table", table, `{"status":"Free"}`, "not permitted")
			m.expect("Dining Table", table, "status", "Occupied")

			w.refused(httpMethodPost, "Kitchen Order", "", `{"reference":"KO-W2","dining_table":"`+table+`","lines":[{"menu_item":"`+item+`","quantity":0}]}`,
				"Quantity must be at least 1 for Juice.")

			w.update("Kitchen Order", order, `{"status":"Preparing"}`)
			w.update("Kitchen Order", order, `{"status":"Served"}`)
			m.expect("Dining Table", table, "status", "Occupied")
		})
	})
}

// runPackCase runs one case as a subtest of the dialect with its own *testing.T, so a failed
// assertion inside it fails that case only (calling Fatal on the dialect's T from a nested
// subtest's goroutine would stop the wrong test).
func runPackCase(f *packFixture, name string, scenario func(f *packFixture)) {
	f.t.Run(name, func(t *testing.T) {
		own := *f
		own.t = t
		scenario(&own)
	})
}
