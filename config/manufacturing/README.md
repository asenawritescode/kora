# manufacturing

Bills of materials, work orders and production runs that move stock: Bill of Materials
(with BOM Component rows), Work Order, Production Run and Material Issue. It builds on the
Selling/Stock pack's `Product` (`config/pos-base`), so install it with that pack.
Roles: **Production Manager** (everything except deleting append-only records) and
**Production Operator** (reads recipes and work orders; creates production runs and material
issues; cannot delete). One view, **Production Overview** at `/production` (open work orders
and bills of materials), and a `manufacturing_overview` report (work orders by status,
quantity made by product).

- A Bill of Materials names the product it makes, how much one batch yields (more than zero)
  and its materials. Once it is Retired, its product and materials are frozen.
- A **Production Run** adds its quantity to the finished product's stock when saved
  (`quantity_effect` on `Product.stock_qty`), atomically with the save. It cannot be edited
  afterwards; a mistake is corrected with a Stock Adjustment from the Stock pack.
- Consuming materials is **not automatic**. Saving a work order or a production run never
  reduces material stock. Each **Material Issue** you record takes its quantity off one
  material's stock in the same step (one material per record, one stock effect per record
  type). It is append-only too.
- Observed on MySQL: stock never goes below zero. An issue for more than is in stock is
  refused and nothing is saved.

Limits: no scheduling, no automatic material consumption, no cost roll-up. A work order
marked Done is not checked against a production run (the Engine cannot enforce that), and
nothing compares issued materials with the recipe. The minimum on a material's quantity is
declared, but the Engine does not check constraints on child table rows, so it is not
enforced. The roles carry no `Product` permission because a pack cannot grant on another
pack's record type; give operators read access to Product through the Selling/Stock roles.

Checks: `go test ./internal/site ./internal/analytics -run Manufacturing` and
`go test -tags integration ./internal/api -run Manufacturing` (needs MySQL, set `KORA_TEST_DSN`).
