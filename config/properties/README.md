# properties

Property letting: Property, Unit, Tenant and Lease.
Roles: **Estate Manager** (everything, including delete) and **Leasing Agent** (reads properties
and units; creates and updates tenants and leases; cannot delete). One view, **Properties
Overview** at `/properties` (units, and active leases), and a properties overview report (units
and leases by status).

- Property names are unique. A unit belongs to a property and carries its asking rent and
  status (Vacant, Occupied, Maintenance).
- Rents and deposits cannot be negative. A lease cannot end before it starts.
- Once a lease is Ended, its unit, tenant and start date are frozen. Other fields (rent,
  end date) can still be corrected.

Limits: overlapping leases on one unit are not prevented, a unit's status is not changed by
a lease (a person sets it), and there are no rent arrears or statements. Use Subscriptions
for recurring rent invoices. The role is named Estate Manager because the legacy
`config/v0/propertymgmt` template already defines a Property Manager role.

Checks: `go test ./internal/site -run Properties`, `go test ./internal/analytics -run Properties`
and `go test -tags integration ./internal/api -run Properties` (needs MySQL, set `KORA_TEST_DSN`).
