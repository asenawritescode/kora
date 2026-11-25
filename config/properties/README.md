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

- An Active lease occupies its unit. Two Active leases of one unit may not overlap (an empty end
  date is open-ended). A new Active lease cannot be let on a unit under Maintenance. Ending a
  lease sets an empty end date to today and vacates the unit unless another Active lease holds
  it. An ended lease cannot be reopened; make a new lease.

Limits: a unit's Maintenance status is set by a person, and ending a lease overwrites it with
Vacant. Deleting a lease does not vacate its unit. There are no rent arrears or statements. Use Subscriptions
for recurring rent invoices. The role is named Estate Manager because the legacy
`config/v0/propertymgmt` template already defines a Property Manager role.

Checks: `go test ./internal/site -run Properties` and the lease rules on both dialects,
`go test -tags integration ./internal/api -run TestPackScriptsLeases` (set
`KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`).
