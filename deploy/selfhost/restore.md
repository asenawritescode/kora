# Self-hosted restore and rollback procedure

Restore only into a newly created, isolated MySQL server and fresh Engine file
volume. Do not import an all-database dump into a running production server or
reuse a volume whose contents have not been verified. SQL restore cannot undo
external payments, messages, or supplier calls made after the backup.

1. Select the backup set and verify its signature and checksums before
   decrypting it:

   ```sh
   cd /secure/restore/kora-backup
   sha256sum -c SHA256SUMS
   ```

   Keep the Engine stopped for the validation and restore steps below. The
   sequence checkpoint is required for backup sets created by the current
   procedure; do not silently skip validation when its manifest is missing.

2. Create a restore-only environment file from the example. Replace every
   placeholder with isolated restore credentials; do not reuse the production
   `.env`. Start only MySQL under a separate Compose project and create the
   empty Engine file volume. Keep this project on its private Compose network
   and do not route customer traffic to it. Ensure the dump came from a
   compatible MySQL version and review database logs before importing.

   ```sh
   cp deploy/selfhost/.env.example deploy/selfhost/restore.env
   # Edit restore.env and generate unique restore secrets.
   docker compose -p kora-restore -f deploy/selfhost/compose.yaml \
     --env-file deploy/selfhost/restore.env up -d --wait --wait-timeout 120 mysql
   docker compose -p kora-restore -f deploy/selfhost/compose.yaml \
     --env-file deploy/selfhost/restore.env exec -T mysql \
     sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=socket -uroot --batch --skip-column-names -e "SELECT 1"'
   docker volume create kora-restore_engine_sites
   ```

   Do not start `engine` or `workspace` yet. The newly initialized account
   credentials are only a starting point; confirm the restored server's actual
   account inventory and authentication before allowing any application
   traffic.
3. Restore the database dump into the new server as its administrator:

   ```sh
   docker compose -p kora-restore -f deploy/selfhost/compose.yaml \
     --env-file deploy/selfhost/restore.env exec -T mysql \
     sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot' < mysql.sql
   ```

   `restore.env` must contain credentials and DSNs for the isolated restore
   stack. Never paste production passwords into command history or this file.
4. Extract the file archive into the new project's empty volume:

   ```sh
   docker run --rm \
     -v kora-restore_engine_sites:/target \
     -v "$PWD:/backup:ro" alpine:3.21 \
     tar -C /target -xzf /backup/engine-sites.tgz
   ```

5. Before starting Engine, validate the restored databases against the
   checkpoint from the same backup set:

   ```sh
   python3 scripts/selfhost-source-manifest.py validate \
     "$PWD/source-sequences.json" \
     --compose-file deploy/selfhost/compose.yaml \
     --env-file deploy/selfhost/restore.env \
     --project kora-restore
   ```

   Validation fails if a database recorded in the backup is missing or its
   restored source sequence is below that backup's high-water mark. A pass
   does not validate external replay sources or establish a retention floor.
   If the operator-maintained retained-backup catalog is available, also
   verify that this exact manifest remains in the catalog and meets its
   database floor before starting Engine:

   ```sh
   python3 scripts/selfhost-source-manifest.py validate-backup \
     /secure/offhost/kora-replay-floor.json \
     "$PWD/source-sequences.json"
   ```

   This check verifies only the backup inventory component; it does not cover
   WAL, outbox, consumer receipts, analytics receipts/checkpoints, or broker
   redelivery and acknowledgments, and it does not permit pruning.
6. Before starting Engine, reconcile MySQL accounts and grants with the
   isolated restore environment. The disposable MySQL 8.4.11 drill found that
   the initialized target `root` and `kora` credentials authenticated after
   `mysqldump --all-databases` restore, while the source credentials did not.
   The tested root/Kora account rows were `kora@%`, `root@%`, and
   `root@localhost`. Treat this as evidence for those tested accounts/version
   only; inspect the target's actual account inventory and compare it with the approved account
   set before opening access. Enumerate account names/hosts without printing
   password hashes, rotate retained accounts to restore-specific secrets, and
   remove unapproved accounts only after confirming no restored component needs
   them. Align `MYSQL_ROOT_PASSWORD` and `KORA_DB_PASSWORD` with the account
   credentials actually installed. Verify intended credentials with an
   authenticated SQL statement such as `SELECT 1`; verify rejected credentials
   return a nonzero SQL-client status. `mysqladmin ping` proves server liveness
   only and may exit successfully even on access denied. Keep the restore
   network isolated until this check and all account reconciliation finish.

   To inventory account names and hosts without selecting password hashes:

   ```sh
   docker compose -p kora-restore -f deploy/selfhost/compose.yaml \
     --env-file deploy/selfhost/restore.env exec -T mysql \
     sh -ec 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=socket -uroot --batch --skip-column-names -e "SELECT User, Host FROM mysql.user ORDER BY User, Host"'
   ```

   Review the complete output against the approved restore account set. Do not
   copy authentication strings or hashes into tickets, logs, or the recovery
   record.
7. Start only the isolated restore stack. Verify `/readyz`, schema/bootstrap
   versions, tenant directory entries, representative tenant login and files,
   pending durable work, and external-effect reconciliation records. Capture
   exact duration and data-loss interval from this drill.
8. For an authorized cutover, stop writes to the old Engine, take a final
   verified backup if possible, update the operator's database/file-volume
   references to the restored resources, then start one Engine and validate
   readiness and tenant operations before reopening traffic. Keep the old data
   volumes unchanged until the recovery owner signs off.

Rollback after an additive code/schema release: disable new feature starts,
keep workers required by active durable work, route to a compatible Engine
version, and retain new additive tables/columns. Do not downgrade to a binary
that cannot read state already written by the newer version. Restoring SQL and
files is a separate disaster-recovery action with an explicit data-loss window;
reconcile all external side effects before resuming work.
