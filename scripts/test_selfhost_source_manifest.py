import importlib.util
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest


MODULE_PATH = Path(__file__).with_name("selfhost-source-manifest.py")
SPEC = importlib.util.spec_from_file_location("selfhost_source_manifest", MODULE_PATH)
manifest_module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(manifest_module)


class SourceManifestTests(unittest.TestCase):
    def test_capture_rejects_empty_application_database_inventory(self):
        def query(sql):
            if "FROM information_schema.SCHEMATA" in sql:
                return "information_schema\nmysql\nperformance_schema\nsys\n"
            raise AssertionError(f"unexpected query: {sql}")

        with self.assertRaisesRegex(ValueError, "no application databases"):
            manifest_module.capture_manifest(query)

    def test_backup_floor_uses_oldest_complete_manifest_per_database(self):
        old = {
            "format_version": 1,
            "databases": [
                {"database": "tenant_a", "source_sequence_high_water": "12"},
                {"database": "platform", "source_sequence_high_water": "4"},
            ],
        }
        new = {
            "format_version": 1,
            "databases": [
                {"database": "tenant_a", "source_sequence_high_water": "30"},
                {"database": "platform", "source_sequence_high_water": "9"},
            ],
        }
        floor = manifest_module.build_backup_floor([("old", old), ("new", new)])
        self.assertEqual(floor["backup_count"], 2)
        self.assertEqual(floor["database_sequence_floor"], {"platform": "4", "tenant_a": "12"})
        self.assertEqual([item["label"] for item in floor["backup_manifests"]], ["new", "old"])

    def test_backup_floor_rejects_empty_or_inconsistent_inventory(self):
        with self.assertRaisesRegex(ValueError, "at least one"):
            manifest_module.build_backup_floor([])
        old = {"format_version": 1, "databases": [{"database": "tenant_a", "source_sequence_high_water": "2"}]}
        incomplete = {"format_version": 1, "databases": [{"database": "tenant_b", "source_sequence_high_water": "8"}]}
        with self.assertRaisesRegex(ValueError, "same database set"):
            manifest_module.build_backup_floor([("old", old), ("incomplete", incomplete)])

    def test_backup_floor_rejects_regression_and_database_set_changes(self):
        def floor(name, sequence):
            manifest = {
                "format_version": 1,
                "databases": [{"database": name, "source_sequence_high_water": str(sequence)}],
            }
            return manifest_module.build_backup_floor([(f"{name}-{sequence}", manifest)])

        previous = floor("tenant_a", 10)
        regressed = floor("tenant_a", 9)
        with self.assertRaisesRegex(ValueError, "regressed"):
            manifest_module.validate_floor_advance(previous, regressed)
        changed = floor("tenant_b", 10)
        with self.assertRaisesRegex(ValueError, "database set changed"):
            manifest_module.validate_floor_advance(previous, changed)

    def test_backup_restore_must_be_in_catalog_and_at_or_above_floor(self):
        old = {
            "format_version": 1,
            "captured_at": "2026-10-01T00:00:00Z",
            "databases": [{"database": "tenant_a", "source_sequence_high_water": "12"}],
        }
        current = {
            "format_version": 1,
            "captured_at": "2026-10-08T00:00:00Z",
            "databases": [{"database": "tenant_a", "source_sequence_high_water": "30"}],
        }
        floor = manifest_module.build_backup_floor([("old", old), ("current", current)])
        manifest_module.validate_backup_against_floor(floor, old)
        manifest_module.validate_backup_against_floor(floor, current)
        unknown = {**old, "captured_at": "2026-10-02T00:00:00Z"}
        with self.assertRaisesRegex(ValueError, "not in the retained backup catalog"):
            manifest_module.validate_backup_against_floor(floor, unknown)

    def test_backup_restore_rejects_database_set_or_floor_mismatch(self):
        retained = {
            "format_version": 1,
            "databases": [{"database": "tenant_a", "source_sequence_high_water": "12"}],
        }
        floor = manifest_module.build_backup_floor([("retained", retained)])
        older = {
            "format_version": 1,
            "databases": [{"database": "tenant_a", "source_sequence_high_water": "11"}],
        }
        floor["backup_manifests"].append({"label": "older", "sha256": manifest_module.hashlib.sha256(
            manifest_module.json.dumps(older, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()})
        floor["backup_count"] += 1
        with self.assertRaisesRegex(ValueError, "below retained floor"):
            manifest_module.validate_backup_against_floor(floor, older)
        wrong_set = {
            "format_version": 1,
            "databases": [{"database": "tenant_b", "source_sequence_high_water": "12"}],
        }
        floor["backup_manifests"].append({"label": "wrong-set", "sha256": manifest_module.hashlib.sha256(
            manifest_module.json.dumps(wrong_set, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()})
        floor["backup_count"] += 1
        with self.assertRaisesRegex(ValueError, "database set does not match"):
            manifest_module.validate_backup_against_floor(floor, wrong_set)

    def test_backup_floor_rejects_malformed_persisted_catalog(self):
        manifest = {
            "format_version": 1,
            "databases": [{"database": "tenant_a", "source_sequence_high_water": "12"}],
        }
        valid = manifest_module.build_backup_floor([("backup", manifest)])
        malformed = []
        bad_count = dict(valid)
        bad_count["backup_count"] = 2
        malformed.append((bad_count, "backup_count"))
        bad_digest = dict(valid)
        bad_digest["backup_manifests"] = [{"label": "backup", "sha256": "not-a-digest"}]
        malformed.append((bad_digest, "digest"))
        empty_floor = dict(valid)
        empty_floor["database_sequence_floor"] = {}
        malformed.append((empty_floor, "sequence floor"))
        bool_version = dict(valid)
        bool_version["format_version"] = True
        malformed.append((bool_version, "format_version"))
        for floor, message in malformed:
            with self.subTest(message=message), self.assertRaisesRegex(ValueError, message):
                manifest_module.validate_backup_against_floor(floor, manifest)

    def test_backup_floor_write_replaces_atomically_with_private_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "replay-floor.json"
            first = {"format_version": 1, "database_sequence_floor": {"tenant_a": "10"}}
            second = {"format_version": 1, "database_sequence_floor": {"tenant_a": "20"}}
            manifest_module.write_floor(target, first)
            manifest_module.write_floor(target, second)
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            self.assertEqual(manifest_module.json.loads(target.read_text()), second)
            self.assertEqual(list(Path(directory).iterdir()), [target])

    def test_concurrent_backup_floor_publication_cannot_regress(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "replay-floor.json"
            def candidate(sequence):
                manifest = {
                    "format_version": 1,
                    "databases": [{"database": "tenant_a", "source_sequence_high_water": str(sequence)}],
                }
                return manifest_module.build_backup_floor([(f"backup-{sequence}", manifest)])

            manifest_module.publish_backup_floor(target, candidate(10))
            candidates = [candidate(20), candidate(15)]
            barrier = threading.Barrier(2)

            def publish(candidate):
                barrier.wait()
                try:
                    manifest_module.publish_backup_floor(target, candidate)
                    return None
                except ValueError as error:
                    return str(error)

            with ThreadPoolExecutor(max_workers=2) as pool:
                errors = list(pool.map(publish, candidates))
            self.assertEqual(
                manifest_module.json.loads(target.read_text())["database_sequence_floor"]["tenant_a"],
                "20",
            )
            self.assertLessEqual(sum(error is not None for error in errors), 1)

    def test_backup_floor_cli_computes_and_validates_retained_backup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            older = root / "older.json"
            newer = root / "newer.json"
            floor = root / "floor.json"
            older.write_text(manifest_module.json.dumps({
                "format_version": 1,
                "captured_at": "2026-10-01T00:00:00Z",
                "databases": [{"database": "tenant_a", "source_sequence_high_water": "10"}],
            }))
            newer.write_text(manifest_module.json.dumps({
                "format_version": 1,
                "captured_at": "2026-10-08T00:00:00Z",
                "databases": [{"database": "tenant_a", "source_sequence_high_water": "20"}],
            }))
            result = subprocess.run(
                [sys.executable, str(MODULE_PATH), "backup-floor", str(floor), str(older), str(newer)],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(manifest_module.json.loads(floor.read_text())["database_sequence_floor"], {"tenant_a": "10"})
            validation = subprocess.run(
                [sys.executable, str(MODULE_PATH), "validate-backup", str(floor), str(older)],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(validation.returncode, 0, validation.stderr)
            late_old = root / "late-old.json"
            late_old.write_text(manifest_module.json.dumps({
                "format_version": 1,
                "captured_at": "2026-09-24T00:00:00Z",
                "databases": [{"database": "tenant_a", "source_sequence_high_water": "5"}],
            }))
            regression = subprocess.run(
                [sys.executable, str(MODULE_PATH), "backup-floor", str(floor), str(late_old), str(older), str(newer)],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(regression.returncode, 2)
            self.assertIn("regressed", regression.stderr)
            self.assertEqual(manifest_module.json.loads(floor.read_text())["database_sequence_floor"], {"tenant_a": "10"})

    def test_capture_uses_maximum_of_singleton_legacy_and_outbox(self):
        def query(sql):
            if "FROM information_schema.SCHEMATA" in sql:
                return "information_schema\nmysql\nplatform_db\ntenant_a\n"
            if "FROM information_schema.TABLES" in sql:
                if "tenant_a" in sql:
                    return "_kora_tenant_event_sequence\n_kora_event_sequence\n_kora_outbox\n"
                return ""
            if "FROM information_schema.COLUMNS" in sql:
                return "1\n"
            if "_kora_tenant_event_sequence" in sql:
                return "10\n"
            if "_kora_event_sequence" in sql:
                return "11\n"
            if "_kora_outbox" in sql:
                return "12\n"
            raise AssertionError(f"unexpected query: {sql}")

        captured = manifest_module.capture_manifest(query)
        self.assertEqual(captured["format_version"], 1)
        self.assertEqual(
            captured["databases"],
            [
                {"database": "platform_db", "source_sequence_high_water": "0"},
                {"database": "tenant_a", "source_sequence_high_water": "12"},
            ],
        )

    def test_capture_escapes_database_identifiers(self):
        seen = []

        def query(sql):
            seen.append(sql)
            if "FROM information_schema.SCHEMATA" in sql:
                return "tenant`odd\n"
            if "FROM information_schema.TABLES" in sql:
                return "_kora_event_sequence\n"
            if "_kora_event_sequence" in sql:
                return "3\n"
            raise AssertionError(f"unexpected query: {sql}")

        manifest_module.capture_manifest(query)
        self.assertTrue(any("`tenant``odd`._kora_event_sequence" in sql for sql in seen))

    def test_validate_requires_all_backed_up_databases_and_sequence(self):
        manifest = {
            "format_version": 1,
            "databases": [
                {"database": "tenant_a", "source_sequence_high_water": "12"},
                {"database": "tenant_b", "source_sequence_high_water": "0"},
            ],
        }
        self.assertEqual(
            manifest_module.validate_manifest(manifest, {"tenant_a": 12, "tenant_b": 9}),
            [],
        )
        self.assertEqual(
            manifest_module.validate_manifest(manifest, {"tenant_a": 11}),
            [
                "restored source sequence is behind backup checkpoint: tenant_a",
                "missing database from restored backup: tenant_b",
            ],
        )

    def test_validate_rejects_duplicate_and_noncanonical_sequences(self):
        base = {"format_version": 1, "databases": []}
        with self.assertRaisesRegex(ValueError, "at least one application database"):
            manifest_module.validate_manifest(base, {})
        duplicate = {
            **base,
            "databases": [
                {"database": "tenant_a", "source_sequence_high_water": "1"},
                {"database": "tenant_a", "source_sequence_high_water": "2"},
            ],
        }
        with self.assertRaisesRegex(ValueError, "duplicate"):
            manifest_module.validate_manifest(duplicate, {})
        invalid = {
            **base,
            "databases": [{"database": "tenant_a", "source_sequence_high_water": "01"}],
        }
        with self.assertRaisesRegex(ValueError, "invalid source sequence"):
            manifest_module.validate_manifest(invalid, {})

    def test_write_manifest_is_atomic_json(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "source-sequences.json"
            manifest_module.write_manifest(
                target,
                {
                    "format_version": 1,
                    "captured_at": "2026-10-15T00:00:00Z",
                    "databases": [{"database": "kora_platform", "source_sequence_high_water": "0"}],
                },
            )
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            self.assertEqual(manifest_module.json.loads(target.read_text())["format_version"], 1)
            self.assertEqual(list(Path(directory).iterdir()), [target])
            with self.assertRaises(FileExistsError):
                manifest_module.write_manifest(
                    target,
                    {
                        "format_version": 1,
                        "captured_at": "later",
                        "databases": [{"database": "kora_platform", "source_sequence_high_water": "0"}],
                    },
                )
            self.assertEqual(manifest_module.json.loads(target.read_text())["captured_at"], "2026-10-15T00:00:00Z")


if __name__ == "__main__":
    unittest.main()
