import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("sync-contract-consumers.py")
SPEC = importlib.util.spec_from_file_location("sync_contract_consumers", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)


class SourceRevisionTest(unittest.TestCase):
    def test_unrelated_dirty_files_do_not_dirty_contract_source(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.email", "test@example.invalid"], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.name", "Contract test"], check=True)
            (repo / "contract").mkdir()
            (repo / "contract/gen").mkdir(parents=True)
            (repo / "contract/types.go").write_text("package contract\n")
            (repo / "contract/gen/main.go").write_text("package main\n")
            (repo / "README.md").write_text("baseline\n")
            subprocess.run(["git", "-C", str(repo), "add", "."], check=True)
            subprocess.run(["git", "-C", str(repo), "commit", "-qm", "baseline"], check=True)

            baseline_revision = subprocess.run(
                ["git", "-C", str(repo), "rev-parse", "HEAD"],
                check=True, capture_output=True, text=True,
            ).stdout.strip()
            (repo / "README.md").write_text("unrelated change\n")
            subprocess.run(["git", "-C", str(repo), "add", "README.md"], check=True)
            subprocess.run(["git", "-C", str(repo), "commit", "-qm", "unrelated"], check=True)
            revision, tree_state = MODULE.source_revision(repo)
            self.assertEqual(tree_state, "clean")
            self.assertEqual(revision, baseline_revision)

            (repo / "contract/types.go").write_text("package contract // changed\n")
            self.assertEqual(MODULE.source_revision(repo)[1], "uncommitted")

    def test_generated_artifact_commit_is_the_published_revision(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.email", "test@example.invalid"], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.name", "Contract test"], check=True)
            (repo / "contract/generated").mkdir(parents=True)
            (repo / "contract/schema").mkdir(parents=True)
            (repo / "contract/gen").mkdir(parents=True)
            (repo / "contract/types.go").write_text("package contract\n")
            (repo / "contract/gen/main.go").write_text("package main\n")
            (repo / "contract/generated/engine-v2.ts").write_text("first\n")
            (repo / "contract/schema/engine-v2.json").write_text("{}\n")
            subprocess.run(["git", "-C", str(repo), "add", "."], check=True)
            subprocess.run(["git", "-C", str(repo), "commit", "-qm", "contract source"], check=True)
            (repo / "contract/generated/engine-v2.ts").write_text("regenerated\n")
            subprocess.run(["git", "-C", str(repo), "add", "."], check=True)
            subprocess.run(["git", "-C", str(repo), "commit", "-qm", "generated artifact"], check=True)

            published_revision = subprocess.run(
                ["git", "-C", str(repo), "rev-parse", "HEAD"],
                check=True, capture_output=True, text=True,
            ).stdout.strip()
            self.assertEqual(MODULE.source_revision(repo), (published_revision, "clean"))


class AndroidManifestTest(unittest.TestCase):
    def test_android_manifest_pins_authoritative_schema_bytes(self):
        schema = b'{"$defs":{},"title":"Kora Engine v2 wire contracts"}'
        digest = MODULE.sha256(schema)
        manifest = MODULE.render_android_manifest("abc123", "clean", digest).decode()
        self.assertIn('const val sourceRevision = "abc123"', manifest)
        self.assertIn('const val sourceTree = "clean"', manifest)
        self.assertIn(f'const val schemaSHA256 = "{digest}"', manifest)
        self.assertIn('app/src/main/assets/contracts/engine-v2.json', manifest)
        self.assertEqual(MODULE.sha256(schema), digest)

    def test_typescript_and_cloud_pins_include_schema_and_artifact_hashes(self):
        schema_hash = "a" * 64
        artifact_hash = "b" * 64
        typescript = MODULE.render_manifest("d19b735", "clean", artifact_hash, schema_hash).decode()
        self.assertIn(f"schemaSHA256: '{schema_hash}'", typescript)
        self.assertIn(f"artifactSHA256: '{artifact_hash}'", typescript)
        cloud = MODULE.json.loads(MODULE.render_cloud_manifest("d19b735", "clean", artifact_hash, schema_hash))
        self.assertEqual(cloud["source_revision"], "d19b735")
        self.assertEqual(cloud["schema_sha256"], schema_hash)
        self.assertEqual(cloud["typescript_sha256"], artifact_hash)

    def test_untracked_generator_input_dirties_contract_source(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.email", "test@example.invalid"], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.name", "Contract test"], check=True)
            (repo / "README.md").write_text("baseline\n")
            subprocess.run(["git", "-C", str(repo), "add", "README.md"], check=True)
            subprocess.run(["git", "-C", str(repo), "commit", "-qm", "baseline"], check=True)
            (repo / "contract/gen").mkdir(parents=True)
            (repo / "contract/gen/new.go").write_text("package main\n")
            self.assertEqual(MODULE.source_revision(repo)[1], "uncommitted")


if __name__ == "__main__":
    unittest.main()
