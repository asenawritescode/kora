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

Limits: no depreciation and no automatic maintenance reminders: the register shows what is
planned, nothing nudges anyone when a task falls due. Marking a task Done does not change
the asset's status, and a new assignment does not update the asset's "Assigned to"; both are
set by hand. Overlapping assignments of one asset are not checked.

Checks: `go test ./internal/site -run TestAssets`, `go test ./internal/analytics -run TestAssets`
and `go test -tags integration ./internal/api -run TestAssets` (needs MySQL, set `KORA_TEST_DSN`).
