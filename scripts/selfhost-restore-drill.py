#!/usr/bin/env python3
"""Opt-in disposable MySQL backup/restore drill for the self-host runbook."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time


MYSQL_IMAGE = "mysql:8.4.11"


class DrillError(RuntimeError):
    pass


def run(
    command: list[str],
    *,
    password_values: tuple[str, ...],
    check: bool = True,
    stdout=None,
    stdin=None,
) -> subprocess.CompletedProcess:
    if stdout is None and stdin is not None and hasattr(stdin, "read"):
        result = subprocess.run(command, stdin=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    elif stdout is None:
        result = subprocess.run(command, input=stdin, text=True, capture_output=True)
    else:
        result = subprocess.run(command, stdin=stdin, stdout=stdout, stderr=subprocess.PIPE, text=False)
    if check and result.returncode:
        detail = result.stderr or getattr(result, "stdout", b"") or b""
        if isinstance(detail, bytes):
            detail = detail.decode("utf-8", errors="replace")
        for value in password_values:
            detail = detail.replace(value, "[REDACTED]")
        raise DrillError(detail[-3000:] or f"command failed with exit code {result.returncode}")
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--run",
        action="store_true",
        help="explicitly authorize creation of isolated temporary MySQL containers",
    )
    args = parser.parse_args()
    if not args.run:
        parser.error("this live drill creates disposable MySQL containers; pass --run explicitly")

    if not Path("/dev/shm").is_dir() or not os.access("/dev/shm", os.W_OK):
        raise DrillError("/dev/shm must be writable so temporary SQL dumps do not use persistent storage")
    run(["docker", "version", "--format", "{{.Server.Version}}"], password_values=())
    run(["docker", "image", "inspect", MYSQL_IMAGE], password_values=())

    suffix = secrets.token_hex(6)
    projects = [
        f"kora-restore-drill-{suffix}-source",
        f"kora-restore-drill-{suffix}-target",
        f"kora-restore-drill-{suffix}-partial",
    ]
    passwords = {
        "source_root": "src-root-" + secrets.token_urlsafe(18),
        "source_app": "src-app-" + secrets.token_urlsafe(18),
        "target_root": "dst-root-" + secrets.token_urlsafe(18),
        "target_app": "dst-app-" + secrets.token_urlsafe(18),
        "partial_root": "partial-root-" + secrets.token_urlsafe(18),
        "partial_app": "partial-app-" + secrets.token_urlsafe(18),
    }
    password_values = tuple(passwords.values())

    with tempfile.TemporaryDirectory(prefix="kora-selfhost-restore-drill-", dir="/dev/shm") as temp_dir:
        root = Path(temp_dir)
        compose_file = root / "compose.yaml"
        compose_file.write_text(
            """services:
  mysql:
    image: mysql:8.4.11
    network_mode: none
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:?required}
      MYSQL_DATABASE: kora_platform
      MYSQL_USER: kora
      MYSQL_PASSWORD: ${KORA_DB_PASSWORD:?required}
    tmpfs:
      - /var/lib/mysql:rw,size=1g
""",
            encoding="utf-8",
        )
        env_files: list[Path] = []
        for name, root_password, app_password in (
            ("source", passwords["source_root"], passwords["source_app"]),
            ("target", passwords["target_root"], passwords["target_app"]),
            ("partial", passwords["partial_root"], passwords["partial_app"]),
        ):
            env_file = root / f"{name}.env"
            env_file.write_text(
                f"MYSQL_ROOT_PASSWORD={root_password}\nKORA_DB_PASSWORD={app_password}\n",
                encoding="utf-8",
            )
            env_file.chmod(0o600)
            env_files.append(env_file)

        def compose(index: int, *args: str, check: bool = True, stdout=None, stdin=None):
            return run(
                [
                    "docker",
                    "compose",
                    "-p",
                    projects[index],
                    "-f",
                    str(compose_file),
                    "--env-file",
                    str(env_files[index]),
                    *args,
                ],
                password_values=password_values,
                check=check,
                stdout=stdout,
                stdin=stdin,
            )

        def wait_ready(index: int) -> None:
            deadline = time.monotonic() + 120
            while time.monotonic() < deadline:
                probe = compose(
                    index,
                    "exec",
                    "-T",
                    "mysql",
                    "sh",
                    "-ec",
                    'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot --batch --skip-column-names -e "SELECT 1"',
                    check=False,
                )
                if probe.returncode == 0:
                    return
                time.sleep(1)
            raise DrillError("disposable MySQL did not become ready")

        try:
            for index in range(2):
                compose(index, "up", "-d", "mysql")
                wait_ready(index)

            compose(
                0,
                "exec",
                "-T",
                "mysql",
                "sh",
                "-ec",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -e "CREATE DATABASE kora_test; '
                "CREATE TABLE kora_test._kora_event_sequence (last_sequence BIGINT PRIMARY KEY); "
                'INSERT INTO kora_test._kora_event_sequence VALUES (7);"',
            )

            repo_root = Path(__file__).resolve().parent.parent
            manifest = root / "source-sequences.json"
            dump = root / "mysql.sql"
            helper = repo_root / "scripts" / "selfhost-source-manifest.py"
            capture = [
                "python3",
                str(helper),
                "capture",
                str(manifest),
                "--compose-file",
                str(compose_file),
                "--env-file",
                str(env_files[0]),
                "--project",
                projects[0],
            ]
            run(capture, password_values=password_values)
            content = json.loads(manifest.read_text(encoding="utf-8"))
            sequences = {row["database"]: row["source_sequence_high_water"] for row in content["databases"]}
            if sequences.get("kora_test") != "7":
                raise DrillError("captured source-sequence manifest did not contain the expected value")

            dump_command = [
                "docker",
                "compose",
                "-p",
                projects[0],
                "-f",
                str(compose_file),
                "--env-file",
                str(env_files[0]),
                "exec",
                "-T",
                "mysql",
                "sh",
                "-ec",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --all-databases '
                "--single-transaction --routines --events --hex-blob",
            ]
            with dump.open("wb") as output:
                run(dump_command, password_values=password_values, stdout=output)
            if dump.stat().st_size < 1000:
                raise DrillError("all-database SQL dump was unexpectedly small")

            # A partial import may execute some earlier statements, so verify
            # it by the backup checkpoint in a separate disposable database.
            # It cannot alter the complete restore target.
            compose(0, "down", "--remove-orphans")
            compose(2, "up", "-d", "mysql")
            wait_ready(2)
            truncated_dump = root / "mysql-truncated.sql"
            dump_bytes = dump.read_bytes()
            truncated_dump.write_bytes(dump_bytes[: len(dump_bytes) // 2])
            partial_restore = [
                "docker",
                "compose",
                "-p",
                projects[2],
                "-f",
                str(compose_file),
                "--env-file",
                str(env_files[2]),
                "exec",
                "-T",
                "mysql",
                "sh",
                "-ec",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot',
            ]
            with truncated_dump.open("rb") as sql_input:
                run(partial_restore, password_values=password_values, check=False, stdin=sql_input)
            partial_validate = [
                "python3",
                str(helper),
                "validate",
                str(manifest),
                "--compose-file",
                str(compose_file),
                "--env-file",
                str(env_files[2]),
                "--project",
                projects[2],
            ]
            partial_result = run(partial_validate, password_values=password_values, check=False)
            if partial_result.returncode != 1:
                raise DrillError("backup checkpoint did not reject the partial SQL restore")
            compose(2, "down", "--remove-orphans")

            restore_command = [
                "docker",
                "compose",
                "-p",
                projects[1],
                "-f",
                str(compose_file),
                "--env-file",
                str(env_files[1]),
                "exec",
                "-T",
                "mysql",
                "sh",
                "-ec",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot',
            ]
            with dump.open("rb") as sql_input:
                run(restore_command, password_values=password_values, stdin=sql_input)

            validate = [
                "python3",
                str(helper),
                "validate",
                str(manifest),
                "--compose-file",
                str(compose_file),
                "--env-file",
                str(env_files[1]),
                "--project",
                projects[1],
            ]
            run(validate, password_values=password_values)

            # Exercise the runbook's tenant file-volume archive and extract
            # commands using only tmpfs mounts and a synthetic sentinel file.
            # The archive itself stays in the private /dev/shm drill directory.
            file_archive = root / "engine-sites.tgz"
            file_round_trip = (
                "mkdir -p /source/synthetic-tenant/files /target && "
                "printf 'selfhost restore drill sentinel\\n' > /source/synthetic-tenant/files/sentinel.txt && "
                "tar -C /source -czf /backup/engine-sites.tgz . && "
                "tar -C /target -xzf /backup/engine-sites.tgz && "
                "cmp /source/synthetic-tenant/files/sentinel.txt /target/synthetic-tenant/files/sentinel.txt"
            )
            run(
                [
                    "docker", "run", "--rm", "--network", "none",
                    "--tmpfs", "/source:rw,size=16m", "--tmpfs", "/target:rw,size=16m",
                    "--volume", f"{root}:/backup", "alpine:3.21", "sh", "-ec", file_round_trip,
                ],
                password_values=password_values,
            )
            if not file_archive.is_file() or file_archive.stat().st_size == 0:
                raise DrillError("tenant file-volume archive was not created")

            compose(
                1,
                "exec",
                "-T",
                "mysql",
                "sh",
                "-ec",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -e "TRUNCATE TABLE kora_test._kora_event_sequence"',
            )
            stale = run(validate, password_values=password_values, check=False)
            if stale.returncode != 1 or "behind backup checkpoint" not in (stale.stdout + stale.stderr):
                raise DrillError("restore validation did not reject a database below its sequence checkpoint")

            compose(1, "restart", "mysql")
            wait_ready(1)
            account_results = {}
            for role, password in (
                ("source root", passwords["source_root"]),
                ("target root", passwords["target_root"]),
                ("source Kora", passwords["source_app"]),
                ("target Kora", passwords["target_app"]),
            ):
                account = "root" if "root" in role else "kora"
                probe = compose(
                    1,
                    "exec",
                    "-T",
                    "mysql",
                    "sh",
                    "-ec",
                    f'MYSQL_PWD="{password}" mysql --protocol=tcp -h 127.0.0.1 -u{account} --batch --skip-column-names -e "SELECT 1"',
                    check=False,
                )
                account_results[role] = probe.returncode == 0
            if not account_results["target root"] or not account_results["target Kora"]:
                raise DrillError("restore-target root/Kora credentials cannot authenticate after import")
            if account_results["source root"] or account_results["source Kora"]:
                raise DrillError("source credentials unexpectedly authenticate after MySQL 8.4.11 restore")
            inventory = compose(
                1,
                "exec",
                "-T",
                "mysql",
                "sh",
                "-ec",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql --protocol=tcp -h 127.0.0.1 -uroot --batch --skip-column-names -e "SELECT CONCAT(User, CHAR(64), Host) FROM mysql.user WHERE User IN (\'root\', \'kora\') ORDER BY User, Host"',
            )
            accounts = inventory.stdout.strip().splitlines()
            if accounts != ["kora@%", "root@%", "root@localhost"]:
                raise DrillError(f"restored root/Kora account inventory differs from the initialized target: {accounts!r}")
            full_inventory = compose(
                1,
                "exec",
                "-T",
                "mysql",
                "sh",
                "-ec",
                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=socket -uroot --batch --skip-column-names -e "SELECT User, Host FROM mysql.user ORDER BY User, Host"',
            )
            inventory_rows = [line.split("\t", 1) for line in full_inventory.stdout.strip().splitlines()]
            if not inventory_rows or any(len(row) != 2 for row in inventory_rows):
                raise DrillError("restored MySQL account inventory query returned an invalid shape")
            source_liveness = compose(
                1,
                "exec",
                "-T",
                "mysql",
                "sh",
                "-ec",
                f'MYSQL_PWD="{passwords["source_root"]}" mysqladmin ping -h 127.0.0.1 -uroot --silent',
                check=False,
            )
            if source_liveness.returncode != 0:
                raise DrillError("mysqladmin ping no longer demonstrates server liveness with rejected credentials")

            print(
                "Self-host SQL restore drill passed: checkpoint capture, all-database dump/restore, "
                "truncated-restore and stale-sequence rejection, tenant file-volume archive/extract, restart, "
                "SQL-authenticated account probes, and liveness/auth distinction."
            )
            print("SQL account authentication: " + ", ".join(
                f"{role}={'accepted' if accepted else 'rejected'}"
                for role, accepted in account_results.items()
            ))
            print("SQL account inventory (names/hosts only): " + ", ".join(accounts))
            print("Full MySQL account inventory (names/hosts only): " + ", ".join(
                f"{user}@{host}" for user, host in inventory_rows
            ))
            print(f"Temporary SQL dump size: {dump.stat().st_size} bytes; MySQL data stayed on tmpfs.")
        finally:
            for index in reversed(range(3)):
                compose(index, "down", "--remove-orphans", check=False)

    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (DrillError, OSError, subprocess.SubprocessError, json.JSONDecodeError) as error:
        print(f"self-host restore drill failed: {error}", file=sys.stderr)
        raise SystemExit(1)
