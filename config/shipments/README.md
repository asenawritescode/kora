# shipments

Goods in carriage and their tracking trail: Shipment and Shipment Event.
Roles: **Logistics Manager** (everything, including delete) and **Shipping Clerk** (reads, books
and updates shipments and events, cannot delete). One view, **Shipments Board** at `/shipments`,
and a shipments overview report (shipments and charge by status).

- A shipment reference is unique. Weight and charge cannot be negative.
- A shipment cannot be marked Delivered without a delivered-on date. Once Delivered, its
  customer, origin and destination are frozen.
- A shipment event is a dated step (Collected, In transit, Delivered, Failed) with a note.

Limits: shipments and their tracking events. No live tracking, no route optimisation, no
customer tracking page. Events are a log: adding one does not change the shipment's status, and
the customer is typed as a name, not linked to a customer record.

Checks: `go test ./internal/site -run Shipments`, `go test ./internal/analytics -run Shipments`
and `go test -tags integration ./internal/api -run Shipments` (needs MySQL, set `KORA_TEST_DSN`).
