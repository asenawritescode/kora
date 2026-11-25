# shipments

Goods in carriage and their tracking trail: Shipment and Shipment Event.
Roles: **Logistics Manager** (everything, including delete) and **Shipping Clerk** (reads, books
and updates shipments and events, cannot delete). One view, **Shipments Board** at `/shipments`,
and a shipments overview report (shipments and charge by status).

- A shipment reference is unique. Left blank, it is the shipment's number. Weight and charge cannot be negative.
- A shipment cannot be marked Delivered without a delivered-on date. Once Delivered, its
  customer, origin and destination are frozen.
- A shipment event is a dated step (Collected, In transit, Delivered, Failed) with a note.
- A shipment's status moves forward only: Booked, Collected, In transit, then Delivered or Failed.
  Delivered and Failed are final. A person's status change follows the same rule.
- Saving a tracking event moves its shipment to the event's status when that is not backwards.
  A Delivered event fills the shipment's delivered-on date when it is empty. A Delivered or Failed
  shipment takes no more events. An event without a status is a note and changes nothing.
- A tracking event cannot be edited once saved.

Limits: shipments and their tracking events. No live tracking, no route optimisation, no
customer tracking page. The customer is typed as a name, not linked to a customer record.

Checks: `go test ./internal/site -run Shipments`, `go test ./internal/analytics -run Shipments`
and `go test -tags integration ./internal/api -run 'Shipments|TestPackScriptsShipments'` (needs MySQL
and Postgres, set `KORA_TEST_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_*_DSN`).
