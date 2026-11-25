# menu-tables

Menu, tables and kitchen orders for restaurants and cafes: Menu Item, Dining Table,
Kitchen Order (with Kitchen Order Line items).
Roles: **Restaurant Manager** (everything) and **Waiter** (reads the menu and tables,
creates and edits kitchen orders, cannot delete). One view, **Menu and Tables** at `/menu`
(kitchen orders by status, the tables, the menu), and a `menu_tables_insights` dashboard (kitchen
orders by status, menu items by category).

- Table numbers and order references are unique; every order needs at least one line.
- "Items ordered" on an order adds up its line quantities.
- A new order makes its table Occupied (a Free or Reserved table; an Occupied one stays so).
- Every new or changed line needs a quantity of at least 1 and an available menu item. Editing an
  old order does not re-check lines it already had, so an item marked unavailable later does not
  block the edit.
- Cancelling an order requires a reason in the notes.
- Once an order is Served or Cancelled, its reference and table are frozen.
- A menu item can optionally point to a Product from the Selling pack (`erpsell`), which
  must be installed with this pack.

Limits: menu, tables and kitchen orders. No table-bill splitting, no kitchen display, no
recipe costing. Freeing a table is done by hand (a Restaurant Manager sets its status): serving an
order does not free its table, because diners stay at the table after the food is served.
Lines of a served order are not frozen. The minimum price (0) and seats (1) are declared on the
fields but the Engine does not enforce field `min` yet (issue #498), so a negative price or zero
seats is not refused.

Checks: `go test ./internal/site -run Menu`, and on MySQL and PostgreSQL
`go test -tags integration ./internal/api -run 'TestPackScriptsMenuTables|TestMenuKitchenOrderWorkflowOnMySQL'`
(set `KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` / `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`).
