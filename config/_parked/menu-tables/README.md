# menu-tables

Menu, tables and kitchen orders for restaurants and cafes: Menu Item, Dining Table,
Kitchen Order (with Kitchen Order Line items).
Roles: **Restaurant Manager** (everything) and **Waiter** (reads the menu and tables,
creates and edits kitchen orders, cannot delete). One view, **Menu and Tables** at `/menu`
(kitchen orders by status, the tables, the menu), and a `menu_overview` report (kitchen
orders by status, menu items by category).

- Table numbers and order references are unique; every order needs at least one line.
- "Items ordered" on an order adds up its line quantities.
- Cancelling an order requires a reason in the notes.
- Once an order is Served or Cancelled, its reference and table are frozen.
- A menu item can optionally point to a Product from the Selling pack (`pos-base`), which
  must be installed with this pack.

Limits: menu, tables and kitchen orders. No table-bill splitting, no kitchen display, no
recipe costing. Changing an order's status does not change the table's status, and
ordering an item that is marked not available is not refused; both are done by hand.
Lines of a served order are not frozen. The minimum price (0), seats (1) and line quantity (1)
are declared on the fields but the Engine does not enforce field `min` yet (issue #498), so
a negative price or a zero quantity is not refused.

Checks: `go test ./internal/site -run Menu`, `go test ./internal/analytics -run Menu` and
`go test -tags integration ./internal/api -run Menu` (needs MySQL, set `KORA_TEST_DSN`).
