# assets

Equipment and repairs: Asset Category, Asset, Maintenance Task and Asset Assignment.
Roles: **Asset Manager** (everything) and **Asset User** (reads assets and categories,
creates and edits maintenance tasks and assignments, cannot delete). One view, **Asset
Register** at `/assets` (all assets, and the planned maintenance tasks), and an
`assets_overview` report (assets by category, assets by status, tasks by status).

- Asset tags and category names are unique. Assets are found by name, tag or serial number.
- A Maintenance Task cannot be marked Done without a "Done on" date.
- An assignment cannot end before it starts.
- Once an asset is Retired, its tag and purchase date are frozen.

- An assignment that is current today puts the asset In use with its user (an In store asset);
  when it ends, the asset's "Assigned to" clears and an In use asset goes back to In store.
- Assignments of one asset may not overlap; an open-ended assignment runs on indefinitely.
- A new Maintenance Task cannot be added to a Retired asset.
- Marking a task Done returns an Under repair asset to service (In use if it has a holder, else
  In store), unless another planned task on it is already due. Done and Cancelled tasks cannot
  change status.

Limits: no depreciation and no automatic maintenance reminders: the register shows what is
planned, nothing nudges anyone when a task falls due. A planned task does not put an asset
Under repair; that is set by hand.

Checks: `go test ./internal/site -run TestAssets` and
`go test -tags integration ./internal/api -run TestPackScriptsAssets` (needs MySQL and Postgres,
set `KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN` and `KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN`).
