from contextlib import redirect_stderr
import importlib.util
from pathlib import Path
import subprocess
import sys
from unittest import TestCase, main, mock
from io import StringIO


MODULE_PATH = Path(__file__).with_name("selfhost-restore-drill.py")
SPEC = importlib.util.spec_from_file_location("selfhost_restore_drill", MODULE_PATH)
drill = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(drill)


class RestoreDrillSafetyTests(TestCase):
    def test_requires_explicit_opt_in_before_running_docker(self):
        error_output = StringIO()
        with (
            mock.patch.object(sys, "argv", [str(MODULE_PATH)]),
            mock.patch.object(subprocess, "run") as run,
            redirect_stderr(error_output),
            self.assertRaises(SystemExit) as raised,
        ):
            drill.main()

        self.assertEqual(raised.exception.code, 2)
        self.assertIn("--run explicitly", error_output.getvalue())
        run.assert_not_called()


if __name__ == "__main__":
    main()
