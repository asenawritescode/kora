# Cake Business Accounts — Draft Config Pack

Source workbook: `/home/asena/Downloads/Cake_Business_Accounts_Fixed (1).xlsx`

This draft is intentionally outside the repository. It models the workbook as Kora DocTypes using the backend capabilities currently implemented:

- S-expression computed fields: arithmetic, `sum`, `count`, `round`, `if`, comparisons, `today`, `datediff`, and `concat`.
- `linked_field` auto-population from Link fields.
- Child tables and parent aggregates.
- Roles, permissions, and a workspace view.

## Formula mapping

| Workbook area | Draft implementation |
|---|---|
| Item Master F | `Item Master.cost_per_purchase_unit = purchase_cost / purchase_qty` |
| Item Master G/H | Explicit `costing_unit` and `conversion_factor`; `cost_per_costing_unit = cost_per_purchase_unit / conversion_factor` |
| Costing ingredient/decor rows | `Cake Recipe Ingredient` child rows with linked unit cost and computed line cost |
| Costing labour/overhead rows | `Cake Recipe Overhead` child rows with computed line cost |
| Costing totals/profit | Parent `Cake Recipe` `sum` aggregates and arithmetic margin calculations |
| Orders I/K/N/O | `Cake Order` computed totals, balance, order cost, and estimated gross profit |
| Orders L | Manual `payment_status` Select. The backend computed evaluator currently coerces expression results to numeric values, so a computed text status is not used. |
| Expenses D/G/H | Linked category/supplier/unit cost plus quantity × unit cost × explicit unit multiplier |
| Profit & Loss | Not represented as a fake computed DocType: monthly cross-DocType `SUMIFS` needs a report/analytics query. |
| Quarterly Purchases | Not represented as a fake computed DocType: three-month `COUNTIFS`/`SUMIFS` needs a report/analytics query. |

The spreadsheet's flavour/size lookup is represented by selecting a `Cake Recipe` Link on each order. Flavour, size, price, and cost then populate from that linked recipe. This is deterministic and uses the backend's actual link behavior rather than Excel cross-sheet lookup syntax.

The `reports/` directory defines the governed analytics intent for the workbook's cross-DocType reporting. It is kept separate from DocType import until the report registry is wired into site provisioning.

## Import note

This is a review draft only. Do not run `kora config import` until the model and naming are approved. The repository's current CLI import reads DocTypes, roles, permissions, workflows, and views; reporting definitions should be added through the appropriate analytics/report mechanism in a later integration step.
