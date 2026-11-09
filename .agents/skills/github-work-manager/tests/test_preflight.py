import importlib.util
import os
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).parents[1] / "scripts" / "preflight.py"
spec = importlib.util.spec_from_file_location("preflight", SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class PreflightTests(unittest.TestCase):
    def test_dirty_worktree_is_blocked(self):
        with tempfile.TemporaryDirectory() as temp:
            os.system(f"git -C {temp} init -q")
            Path(temp, "user-change.txt").write_text("keep")
            old = os.getcwd()
            try:
                os.chdir(temp)
                result = module.inspect()
            finally:
                os.chdir(old)
        self.assertEqual(result["status"], "BLOCKED")
        self.assertTrue(result["dirty"])


if __name__ == "__main__":
    unittest.main()
