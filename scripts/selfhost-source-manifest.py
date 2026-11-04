#!/usr/bin/env python3
"""Capture and verify source-sequence high-water marks for self-host backups."""

from __future__ import annotations

import argparse
import fcntl
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from typing import Callable


FORMAT_VERSION = 1
FLOOR_FORMAT_VERSION = 1
SYSTEM_DATABASES = {"information_schema", "mysql", "performance_schema", "sys"}
DECIMAL = re.compile(r"(?:0|[1-9][0-9]*)\Z")


def _sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def _sql_identifier(value: str) -> str:
    return "`" + value.replace("`", "``") + "`"


def _parse_nonnegative(value: str, context: str) -> int:
    if not DECIMAL.fullmatch(value):
        raise ValueError(f"invalid nonnegative sequence in {context}")
    return int(value)


def capture_manifest(query: Callable[[str], str]) -> dict:
    schemas = [
        line for line in query(
            "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA "
            "ORDER BY SCHEMA_NAME"
        ).splitlines()
        if line and line not in SYSTEM_DATABASES
    ]
    if not schemas:
        raise ValueError("no application databases found for backup checkpoint")
    databases = []
    for schema in schemas:
        identifier = _sql_identifier(schema)
        schema_literal = _sql_literal(schema)
        tables = {
            line for line in query(
                "SELECT TABLE_NAME FROM information_schema.TABLES "
                f"WHERE TABLE_SCHEMA = {schema_literal}"
            ).splitlines()
        }
        high_water = 0
        if "_kora_tenant_event_sequence" in tables:
            result = query(
                f"SELECT COALESCE(MAX(last_sequence), 0) "
                f"FROM {identifier}._kora_tenant_event_sequence"
            ).strip()
            high_water = max(high_water, _parse_nonnegative(result or "0", schema))
        if "_kora_event_sequence" in tables:
            result = query(
                f"SELECT COALESCE(MAX(last_sequence), 0) "
                f"FROM {identifier}._kora_event_sequence"
            ).strip()
            high_water = max(high_water, _parse_nonnegative(result or "0", schema))
        if "_kora_outbox" in tables:
            has_sequence = query(
                "SELECT COUNT(*) FROM information_schema.COLUMNS "
                f"WHERE TABLE_SCHEMA = {schema_literal} "
                "AND TABLE_NAME = '_kora_outbox' "
                "AND COLUMN_NAME = 'source_sequence'"
            ).strip()
            if _parse_nonnegative(has_sequence, f"{schema} outbox schema"):
                result = query(
                    f"SELECT COALESCE(MAX(source_sequence), 0) "
                    f"FROM {identifier}._kora_outbox"
                ).strip()
                high_water = max(high_water, _parse_nonnegative(result or "0", schema))
        databases.append({"database": schema, "source_sequence_high_water": str(high_water)})
    return {
        "format_version": FORMAT_VERSION,
        "captured_at": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
        "databases": databases,
    }


def validate_manifest(manifest: dict, current: dict[str, int]) -> list[str]:
    if (
        not isinstance(manifest, dict)
        or isinstance(manifest.get("format_version"), bool)
        or manifest.get("format_version") != FORMAT_VERSION
    ):
        raise ValueError("unsupported or invalid source manifest format_version")
    entries = manifest.get("databases")
    if not isinstance(entries, list) or not entries:
        raise ValueError("source manifest must contain at least one application database")
    seen: set[str] = set()
    errors = []
    for entry in entries:
        if not isinstance(entry, dict) or not isinstance(entry.get("database"), str):
            raise ValueError("source manifest database entry is malformed")
        name = entry["database"]
        if name in seen:
            raise ValueError("source manifest contains a duplicate database")
        seen.add(name)
        expected_raw = entry.get("source_sequence_high_water")
        if not isinstance(expected_raw, str) or not DECIMAL.fullmatch(expected_raw):
            raise ValueError(f"invalid source sequence high-water for {name}")
        if name not in current:
            errors.append(f"missing database from restored backup: {name}")
        elif current[name] < int(expected_raw):
            errors.append(f"restored source sequence is behind backup checkpoint: {name}")
    return errors


def build_backup_floor(manifests: list[tuple[str, dict]]) -> dict:
    """Compute the per-database minimum for a complete retained-backup inventory.

    Completeness of a remote inventory and deletion of retired backup objects
    remain operator responsibilities; callers must provide every restorable
    backup set from every configured location.
    """
    if not manifests:
        raise ValueError("backup inventory must contain at least one manifest")
    floors: dict[str, int] | None = None
    digests = []
    for label, manifest in manifests:
        if not label:
            raise ValueError("backup manifest label must be nonempty")
        if (
            not isinstance(manifest, dict)
            or isinstance(manifest.get("format_version"), bool)
            or manifest.get("format_version") != FORMAT_VERSION
        ):
            raise ValueError(f"unsupported or invalid source manifest format_version: {label}")
        entries = manifest.get("databases")
        if not isinstance(entries, list) or not entries:
            raise ValueError(f"backup manifest has no database entries: {label}")
        values: dict[str, int] = {}
        for entry in entries:
            if not isinstance(entry, dict) or not isinstance(entry.get("database"), str) or not entry["database"]:
                raise ValueError(f"backup manifest database entry is malformed: {label}")
            name = entry["database"]
            if name in values:
                raise ValueError(f"backup manifest contains duplicate database {name}: {label}")
            raw = entry.get("source_sequence_high_water")
            if not isinstance(raw, str) or not DECIMAL.fullmatch(raw):
                raise ValueError(f"invalid source sequence high-water for {name}: {label}")
            values[name] = int(raw)
        if floors is None:
            floors = values
        elif values.keys() != floors.keys():
            raise ValueError(f"backup manifests do not contain the same database set: {label}")
        else:
            floors = {name: min(floors[name], values[name]) for name in floors}
        encoded = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode("utf-8")
        digests.append({"label": label, "sha256": hashlib.sha256(encoded).hexdigest()})
    assert floors is not None
    return {
        "format_version": FLOOR_FORMAT_VERSION,
        "computed_at": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
        "backup_count": len(manifests),
        "backup_manifests": sorted(digests, key=lambda item: item["label"]),
        "database_sequence_floor": {name: str(value) for name, value in sorted(floors.items())},
    }


def validate_floor_advance(previous: dict | None, candidate: dict) -> None:
    """Reject floor regressions or changes to the tenant database set."""
    _validate_backup_floor(candidate)
    if previous is None:
        return
    _validate_backup_floor(previous, label="previous backup floor")
    prior = previous["database_sequence_floor"]
    current = candidate.get("database_sequence_floor")
    if prior.keys() != current.keys():
        raise ValueError("backup floor database set changed; operator reconciliation is required")
    for name, raw in prior.items():
        next_raw = current[name]
        if not isinstance(raw, str) or not DECIMAL.fullmatch(raw):
            raise ValueError(f"invalid previous source-sequence floor for {name}")
        if not isinstance(next_raw, str) or not DECIMAL.fullmatch(next_raw):
            raise ValueError(f"invalid candidate source-sequence floor for {name}")
        if int(next_raw) < int(raw):
            raise ValueError(f"backup source-sequence floor regressed for {name}")


def _validate_backup_floor(floor: dict, label: str = "backup floor") -> None:
    if (
        not isinstance(floor, dict)
        or isinstance(floor.get("format_version"), bool)
        or floor.get("format_version") != FLOOR_FORMAT_VERSION
    ):
        raise ValueError(f"unsupported or invalid {label} format_version")
    databases = floor.get("database_sequence_floor")
    catalog = floor.get("backup_manifests")
    count = floor.get("backup_count")
    if not isinstance(databases, dict) or not databases:
        raise ValueError(f"{label} is missing its database sequence floor")
    if not isinstance(catalog, list) or not catalog:
        raise ValueError(f"{label} is missing its manifest catalog")
    if isinstance(count, bool) or not isinstance(count, int) or count != len(catalog):
        raise ValueError(f"{label} backup_count does not match its manifest catalog")
    for name, raw in databases.items():
        if not isinstance(name, str) or not name or not isinstance(raw, str) or not DECIMAL.fullmatch(raw):
            raise ValueError(f"{label} has an invalid source-sequence floor")
    labels: set[str] = set()
    for entry in catalog:
        if not isinstance(entry, dict):
            raise ValueError(f"{label} has a malformed manifest catalog entry")
        backup_label = entry.get("label")
        digest = entry.get("sha256")
        if not isinstance(backup_label, str) or not backup_label or backup_label in labels:
            raise ValueError(f"{label} has an invalid or duplicate manifest label")
        if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise ValueError(f"{label} has an invalid manifest digest")
        labels.add(backup_label)


def validate_backup_against_floor(floor: dict, manifest: dict) -> None:
    """Require a restore candidate to appear in the retained backup catalog."""
    _validate_backup_floor(floor)
    databases = floor["database_sequence_floor"]
    catalog = floor["backup_manifests"]
    if (
        not isinstance(manifest, dict)
        or isinstance(manifest.get("format_version"), bool)
        or manifest.get("format_version") != FORMAT_VERSION
    ):
        raise ValueError("unsupported or invalid source manifest format_version")
    encoded = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode("utf-8")
    digest = hashlib.sha256(encoded).hexdigest()
    if not any(isinstance(item, dict) and item.get("sha256") == digest for item in catalog):
        raise ValueError("backup manifest is not in the retained backup catalog")
    entries = manifest.get("databases")
    if not isinstance(entries, list):
        raise ValueError("source manifest databases must be an array")
    names = []
    for entry in entries:
        if not isinstance(entry, dict) or not isinstance(entry.get("database"), str) or not entry["database"]:
            raise ValueError("source manifest database entry is malformed")
        names.append(entry["database"])
    if len(names) != len(set(names)):
        raise ValueError("source manifest contains duplicate database entries")
    if set(names) != databases.keys():
        raise ValueError("backup manifest database set does not match retained floor")
    values: dict[str, int] = {}
    for entry in entries:
        if not isinstance(entry, dict) or not isinstance(entry.get("database"), str):
            raise ValueError("source manifest database entry is malformed")
        name = entry["database"]
        if name in values:
            raise ValueError(f"source manifest contains duplicate database: {name}")
        raw = entry.get("source_sequence_high_water")
        floor_raw = databases.get(name)
        if not isinstance(raw, str) or not DECIMAL.fullmatch(raw):
            raise ValueError(f"invalid source sequence high-water for {name}")
        if not isinstance(floor_raw, str) or not DECIMAL.fullmatch(floor_raw):
            raise ValueError(f"backup floor is missing a valid sequence for {name}")
        values[name] = int(raw)
        if values[name] < int(floor_raw):
            raise ValueError(f"backup source sequence is below retained floor for {name}")
    if values.keys() != databases.keys():
        raise ValueError("backup manifest database set does not match retained floor")


def write_manifest(path: Path, manifest: dict) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(manifest, stream, sort_keys=True, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        # Publish without replacing an existing backup artifact. A repeated
        # capture must choose a new backup directory instead of overwriting
        # the checkpoint paired with an earlier dump.
        os.link(temp_name, path)
        os.unlink(temp_name)
        dir_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if os.path.exists(temp_name):
            os.unlink(temp_name)


def write_floor(path: Path, floor: dict) -> None:
    """Atomically publish the operator-owned backup floor with mode 0600."""
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(floor, stream, sort_keys=True, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_name, path)
        dir_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if os.path.exists(temp_name):
            os.unlink(temp_name)


def publish_backup_floor(path: Path, candidate: dict) -> None:
    """Serialize floor advances so concurrent operators cannot regress it."""
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    lock_path = path.with_name(path.name + ".lock")
    lock_fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o600)
    try:
        os.fchmod(lock_fd, 0o600)
        fcntl.flock(lock_fd, fcntl.LOCK_EX)
        previous = json.loads(path.read_text(encoding="utf-8")) if path.exists() else None
        validate_floor_advance(previous, candidate)
        write_floor(path, candidate)
    finally:
        fcntl.flock(lock_fd, fcntl.LOCK_UN)
        os.close(lock_fd)


def mysql_query(compose_file: Path, env_file: Path, project: str | None, sql: str) -> str:
    command = ["docker", "compose"]
    if project:
        command.extend(["-p", project])
    command.extend([
        "-f", str(compose_file), "--env-file", str(env_file), "exec", "-T", "mysql",
        "sh", "-ec", 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --batch --skip-column-names --raw',
    ])
    result = subprocess.run(command, input=sql, text=True, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError("MySQL checkpoint query failed: " + result.stderr.strip())
    return result.stdout.strip()


def current_sequences(query: Callable[[str], str]) -> dict[str, int]:
    manifest = capture_manifest(query)
    return {
        entry["database"]: int(entry["source_sequence_high_water"])
        for entry in manifest["databases"]
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="action", required=True)
    for action in ("capture", "validate"):
        command = subparsers.add_parser(action)
        command.add_argument("manifest", type=Path)
        command.add_argument("--compose-file", type=Path, required=True)
        command.add_argument("--env-file", type=Path, required=True)
        command.add_argument("--project", help="Compose project name, if not the default")
    floor_parser = subparsers.add_parser("backup-floor")
    floor_parser.add_argument("floor", type=Path, help="operator-owned floor file to create or update")
    floor_parser.add_argument("manifests", type=Path, nargs="+", help="every retained backup's source-sequences.json")
    verify_parser = subparsers.add_parser("validate-backup")
    verify_parser.add_argument("floor", type=Path)
    verify_parser.add_argument("manifest", type=Path)
    args = parser.parse_args()
    try:
        if args.action == "backup-floor":
            loaded = [(str(path), json.loads(path.read_text(encoding="utf-8"))) for path in args.manifests]
            candidate = build_backup_floor(loaded)
            publish_backup_floor(args.floor, candidate)
            print(f"computed oldest-restorable backup floor from {candidate['backup_count']} manifests")
            return 0
        if args.action == "validate-backup":
            floor = json.loads(args.floor.read_text(encoding="utf-8"))
            manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
            validate_backup_against_floor(floor, manifest)
            print("backup manifest is present in the retained catalog and meets its floor")
            return 0
        query = lambda sql: mysql_query(args.compose_file, args.env_file, args.project, sql)
        if args.action == "capture":
            write_manifest(args.manifest, capture_manifest(query))
            print(f"captured source-sequence checkpoint: {args.manifest}")
            return 0
        manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
        errors = validate_manifest(manifest, current_sequences(query))
        if errors:
            for error in errors:
                print(error, file=sys.stderr)
            return 1
        print("restored databases meet the backup source-sequence checkpoint")
        return 0
    except (OSError, json.JSONDecodeError, ValueError, RuntimeError) as error:
        print(str(error), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
