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
            (repo / "cmd/contractgen").mkdir(parents=True)
            (repo / "contract/types.go").write_text("package contract\n")
            (repo / "cmd/contractgen/main.go").write_text("package main\n")
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

    def test_untracked_generator_input_dirties_contract_source(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            subprocess.run(["git", "init", "-q", str(repo)], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.email", "test@example.invalid"], check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.name", "Contract test"], check=True)
            (repo / "README.md").write_text("baseline\n")
            subprocess.run(["git", "-C", str(repo), "add", "README.md"], check=True)
            subprocess.run(["git", "-C", str(repo), "commit", "-qm", "baseline"], check=True)
            (repo / "cmd/contractgen").mkdir(parents=True)
            (repo / "cmd/contractgen/new.go").write_text("package main\n")
            self.assertEqual(MODULE.source_revision(repo)[1], "uncommitted")


if __name__ == "__main__":
    unittest.main()
