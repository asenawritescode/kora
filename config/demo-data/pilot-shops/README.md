# Pilot shop demo data

**Fabricated demo data for M1 testing. These are not real businesses, people or products.**
Names, emails (`@example.com`), phone numbers and barcodes are made up. Barcodes are checksum-valid EAN-13 codes with the `616` prefix but are not registered with GS1.

Used by:
- asenawritescode/kora#116 (P7b: POS package, KES defaults, demo catalogue)
- asenawritescode/kora-cloud#23 (X2: provision the 3 pilot tenants)

| Shop | Type | Items | Categories | Registers | Users |
|---|---|---|---|---|---|
| `duka-mama-wanjiku` | Neighbourhood duka | 60 | 15 | 1 | Owner, Cashier |
| `minimart-baraka` | Minimart | 110 | 10 | 2 | Owner, Manager, 3 Cashiers |
| `wholesale-kilimo` | Wholesaler and agrovet | 69 | 15 | 1 | Owner, Manager, Cashier |

Each shop folder has:
- `items.csv`: item_code, item_name, category, uom, barcode, selling_price, cost_price, reorder_level
- `opening_stock.csv`: item_code, location, qty, unit_cost
- `shop.yaml`: shop name, currency, timezone, locations, registers with opening float, users, naming series prefix

Prices are KES with 2 decimals, with cost below selling price. A few items per shop have zero stock or stock below the reorder level so low-stock reports have data. Field names follow the planned P1a Item fields (kora#106) and may be adjusted when P1a lands.
