# Self-hosted backup procedure

This Compose example uses a dedicated MySQL 8.4.11 server and the named
`mysql_data` and `engine_sites` volumes. The standalone UI image is built from
the sibling `kora-studio-mvp` checkout. Keep database dumps and file archives
off the database host, encrypt them before upload, restrict access, and test
restores on an isolated target.

The supported recovery point is the interval between completed backup sets.
This procedure quiesces Engine writes so the SQL dump and file volume describe
one consistent point. It does not promise zero data loss or a recovery-time
objective; measure those in a restore drill.

1. Copy `.env.example` to `.env`, replace the secrets, and make sure `.env` is
   excluded from backups and version control.
2. Stop only Engine writes while leaving MySQL available:

   ```sh
   docker compose -f deploy/selfhost/compose.yaml --env-file deploy/selfhost/.env stop engine
   ```

3. Create a private backup directory on a mounted off-host staging location,
   capture the tenant source-sequence checkpoints, then dump all databases
   and archive the Engine file volume. Engine must remain stopped between the
   checkpoint and the completed database/file backup set:

   ```sh
   backup_dir=/secure/offhost/kora-$(date -u +%Y%m%dT%H%M%SZ)
   install -d -m 0700 "$backup_dir"
   python3 scripts/selfhost-source-manifest.py capture \
     "$backup_dir/source-sequences.json" \
     --compose-file deploy/selfhost/compose.yaml \
     --env-file deploy/selfhost/.env
   docker compose -f deploy/selfhost/compose.yaml --env-file deploy/selfhost/.env \
     exec -T mysql sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --all-databases --single-transaction --routines --events --hex-blob' \
     > "$backup_dir/mysql.sql"
   docker run --rm \
     -v kora-selfhost_engine_sites:/source:ro \
     -v "$backup_dir:/backup" alpine:3.21 \
     tar -C /source -czf /backup/engine-sites.tgz .
   sha256sum "$backup_dir/mysql.sql" "$backup_dir/engine-sites.tgz" \
     "$backup_dir/source-sequences.json" > "$backup_dir/SHA256SUMS"
   ```

4. Encrypt the dump, archive, source-sequence checkpoint, and checksum manifest
   with the operator's approved key before copying them to durable off-host
   storage. Verify the checksums after transfer and retain the key separately.
5. Restart Engine and verify `/readyz` from inside the Engine container, then
   check tenant login, file download, and DB/resource metrics before declaring
   the backup cycle complete.

The example archive command assumes Docker's default Compose project name
`kora-selfhost`. If `-p` or `COMPOSE_PROJECT_NAME` is used, replace the volume
name with the actual `*_engine_sites` volume. Never use `docker compose down -v`
as a backup or cleanup step on a deployment containing data.

The versioned `source-sequences.json` contains database names and decimal
source-sequence high-water marks; it contains no connection credentials. Keep
it in the same encrypted, checksummed backup set. The checkpoint helps detect
a missing or older restored database. It is not a replay-retention floor and
does not permit pruning outbox rows, receipts, checkpoints, WAL files, or old
backup sets.

To record the database portion of the oldest-restorable-backup boundary,
recompute the floor from the manifests belonging to **every** restorable
backup set in **every** configured backup location. Supply the complete list
of decrypted manifests to the helper; decrypt them only into an access-
restricted, preferably memory-backed working directory:

```sh
python3 scripts/selfhost-source-manifest.py backup-floor \
  /secure/offhost/kora-replay-floor.json \
  /run/backup-manifests/kora-20261001/source-sequences.json \
  /run/backup-manifests/kora-20261008/source-sequences.json
```

The helper requires every manifest to describe the same database set, computes
the minimum high-water mark per database, records the manifest digests, writes
atomically with mode `0600`, and serializes concurrent updates. Keep the floor
file and its `.lock` file in protected off-host operator state; do not put them
inside a single backup set. Remove an expired backup object from all recovery
locations and verify it is no longer restorable **before** recomputing the
floor. A failed or interrupted update leaves the previous lower floor in
place. The helper rejects regressions and tenant-database-set changes so those
require explicit operator reconciliation.

This is only a backup-inventory checkpoint. The operator must ensure the input
list is complete; the helper cannot discover omitted remote backup copies,
retention locks, or replicas. The resulting value does not include local WAL,
outbox delivery/consumer receipts, analytics receipts/checkpoints, or broker
redelivery/acknowledgments. It therefore still does not authorize pruning any
durable source or correctness record.

For the disposable MySQL integration drill, install/start Docker and make the
pinned `mysql:8.4.11` image available, then run:

```sh
make check-selfhost-restore-live
```

The opt-in drill creates uniquely named Compose projects with no published
ports and tmpfs-only database filesystems. It captures and restores an
all-database dump, checks the source-sequence checkpoint, rejects a truncated
SQL restore in a separate disposable database, exercises the tenant file
archive/extract commands with a synthetic sentinel on tmpfs, and tests
root/Kora credentials with authenticated SQL queries. On the pinned MySQL
8.4.11 image the initialized target credentials authenticate while the source
credentials are rejected; the target account rows are
`kora@%`, `root@%`, and `root@localhost`. `mysqladmin ping` is not an
authentication test. The drill removes only its own containers and uses
`/dev/shm` for credentials, manifest, SQL dumps, and the file archive. The
synthetic file check validates tar archive/extract behavior, not recovery of
an operator's actual Engine file volume. The drill also does not exercise an
Engine/UI cutover, off-host encryption, or recovery time objectives; those
still require a separately operated restore rehearsal.

On restore, check that the selected manifest is present in the retained
catalog:

```sh
python3 scripts/selfhost-source-manifest.py validate-backup \
  /secure/offhost/kora-replay-floor.json \
  "$PWD/source-sequences.json"
```
