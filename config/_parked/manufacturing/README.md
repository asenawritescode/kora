# manufacturing

Bills of materials, work orders and production runs that move stock: Bill of Materials
(with BOM Component rows), Work Order, Production Run and Material Issue. It builds on the
Selling/Stock pack's `Product` (`config/erpsell`), so install it with that pack.
Roles: **Production Manager** (everything except deleting append-only records) and
**Production Operator** (reads recipes and work orders; creates production runs and material
issues; cannot delete). One view, **Production Overview** at `/production` (open work orders
and bills of materials), and a `manufacturing_insights` dashboard (work orders by status,
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
- A work order's recipe must be Active when the order is created or its recipe changes.
  A production run or material issue needs its work order Planned or In progress; a
  planned order becomes In progress. A Done or Cancelled order cannot change status.
- A production run must make the recipe's product. Each run adds its quantity to the work
  order's **Quantity made** (`quantity_produced`), which only runs set.
- A work order can be marked Done only once Quantity made reaches its quantity.
- A recipe cannot list its own product as a material, and each material needs a quantity
  above zero.

Limits: no scheduling, no automatic material consumption, no cost roll-up. Nothing compares
issued materials with the recipe. The roles carry no `Product` permission because a pack
cannot grant on another pack's record type; give operators read access to Product through
the Selling/Stock roles.

Checks: `go test -count=1 ./internal/site -run Manufacturing` and
`go test -tags integration -count=1 ./internal/api -run 'TestPackScriptsManufacturing|TestManufacturing'`
(needs MySQL and Postgres; see `KORA_PACK_SCRIPTS_DISPOSABLE_*_DSN`).
