import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest


SPEC = importlib.util.spec_from_file_location("release", Path(__file__).with_name("release.py"))
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)


class ParseTagTest(unittest.TestCase):
    def test_versions(self):
        cases = [("v1.0.0", "1.0.0"), ("v0.9.5", "0.9.5"), ("v1.0.0-alpha.2", "1.0.0-alpha.2")]
        for tag, version in cases:
            self.assertEqual(MODULE.parse_tag(tag), version)

    def test_rejects_malformed_tags(self):
        for tag in ("1.0.0", "v1.0", "v1.0.0.1", "vx", "", "v1.0.0 "):
            with self.assertRaises(MODULE.ReleaseError, msg=tag):
                MODULE.parse_tag(tag)


class ChangelogTest(unittest.TestCase):
    SUBJECTS = [
        "feat(crm): publishable CRM module (#507)",
        "fix(schema): unique index on an existing table",
        "refactor: stop exported names repeating their package",
        "docs: describe the release flow",
        "chore: bump a dependency",
        "Return an empty module catalog for a tenant that has never installed a module",
        "security(auth): reject expired handoff codes",
    ]

    def test_commit_type(self):
        self.assertEqual(MODULE.commit_type("feat(crm): x"), "feat")
        self.assertEqual(MODULE.commit_type("Fix: x"), "fix")
        self.assertEqual(MODULE.commit_type("feat!: x"), "feat")
        self.assertEqual(MODULE.commit_type("Return an empty catalog"), "")

    def test_groups_by_type_and_drops_untyped_and_chore(self):
        entry = MODULE.changelog_entry("v1.0.0", "2026-11-13", self.SUBJECTS, "v1.0.0-alpha.2", "o/r")
        self.assertTrue(entry.startswith("## v1.0.0 — 2026-11-13\n"))
        self.assertIn("### Features\n- feat(crm): publishable CRM module\n", entry)
        self.assertIn("### Fixes\n- fix(schema): unique index on an existing table\n", entry)
        self.assertIn("### Security\n- security(auth): reject expired handoff codes\n", entry)
        self.assertIn("### Improvements\n- refactor: stop exported names repeating their package\n", entry)
        self.assertIn("### Documentation\n- docs: describe the release flow\n", entry)
        self.assertNotIn("bump a dependency", entry)
        self.assertNotIn("Return an empty module catalog", entry)
        self.assertIn("github.com/o/r/compare/v1.0.0-alpha.2...v1.0.0", entry)

    def test_first_release_has_no_diff_link_and_prepend_keeps_history(self):
        entry = MODULE.changelog_entry("v0.1.0", "2026-01-01", [], "", "o/r")
        self.assertNotIn("Full diff", entry)
        self.assertEqual(MODULE.prepend("## new\n", "## old\n"), "## new\n\n\n## old\n")
        self.assertEqual(MODULE.prepend("## new\n", ""), "## new\n")


class RepoSlugTest(unittest.TestCase):
    def slug_for(self, url):
        with tempfile.TemporaryDirectory() as directory:
            subprocess.run(["git", "init", "-q", directory], check=True)
            subprocess.run(["git", "-C", directory, "remote", "add", "up", url], check=True)
            saved = (os.getcwd(), MODULE.REMOTE)
            os.chdir(directory)
            MODULE.REMOTE = "up"
            try:
                return MODULE.repo_slug()
            finally:
                os.chdir(saved[0])
                MODULE.REMOTE = saved[1]

    def test_slug_comes_from_the_release_remote_not_origin(self):
        self.assertEqual(self.slug_for("git@github.com:asenawritescode/kora.git"), "asenawritescode/kora")
        self.assertEqual(self.slug_for("https://github.com/asenawritescode/kora"), "asenawritescode/kora")
        self.assertEqual(self.slug_for("https://github.com/asenawritescode/kora.git/"), "asenawritescode/kora")

    def test_a_non_github_remote_needs_an_explicit_repo(self):
        with self.assertRaisesRegex(MODULE.ReleaseError, "KORA_RELEASE_REPO"):
            self.slug_for("/tmp/some/local/path.git")


class CiGateTest(unittest.TestCase):
    def test_gate(self):
        ok = [{"name": "Go", "conclusion": "success"}, {"name": "Lint", "conclusion": "success"},
              {"name": "Cross-repository contract conformance", "conclusion": "failure"}]
        self.assertEqual(MODULE.ci_gate(ok), "")
        red = [{"name": "Go", "conclusion": "failure"}, {"name": "Lint", "conclusion": "success"}]
        self.assertIn("'Go' is failure", MODULE.ci_gate(red))
        running = [{"name": "Go", "conclusion": None}, {"name": "Lint", "conclusion": "success"}]
        self.assertIn("still running", MODULE.ci_gate(running))
        self.assertIn("no 'Lint' job", MODULE.ci_gate([{"name": "Go", "conclusion": "success"}]))


def git(cwd, *args):
    return subprocess.run(["git", "-C", str(cwd), *args], check=True, capture_output=True, text=True).stdout.strip()


class EndToEndTest(unittest.TestCase):
    """Runs prepare and tag against a temporary bare remote with stub gh and go commands."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.remote = root / "remote.git"
        subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(self.remote)], check=True)
        self.work = root / "work"
        subprocess.run(["git", "clone", "-q", str(self.remote), str(self.work)], check=True, capture_output=True)
        git(self.work, "remote", "rename", "origin", "up")
        for key, value in (("user.name", "Release test"), ("user.email", "test@example.invalid")):
            git(self.work, "config", key, value)
        (self.work / "VERSION").write_text("0.9.0\n")
        (self.work / "CHANGELOG.md").write_text("## v0.9.0 — 2026-01-01\n")
        git(self.work, "add", ".")
        git(self.work, "commit", "-q", "-m", "chore: start")
        git(self.work, "tag", "-a", "v0.9.0", "-m", "v0.9.0")
        (self.work / "a.txt").write_text("a")
        git(self.work, "add", ".")
        git(self.work, "commit", "-q", "-m", "feat(crm): add a thing (#1)")
        git(self.work, "push", "-q", "up", "main", "v0.9.0")
        self.bin = root / "bin"
        self.bin.mkdir()
        self.stub("go", "#!/bin/sh\nexit 0\n")
        self.stub("gh", """#!/bin/sh
case "$1 $2" in
  "pr create") echo https://example.invalid/pull/1 ;;
  "run list") echo "${STUB_RUN:-123 completed}" ;;
  "api"*) echo "${STUB_JOBS}" ;;
esac
""")
        self.saved = (os.getcwd(), os.environ.get("PATH"), MODULE.REMOTE, MODULE.MAIN)
        os.environ["PATH"] = f"{self.bin}:{os.environ['PATH']}"
        os.environ["KORA_RELEASE_REPO"] = "owner/repo"
        MODULE.REMOTE, MODULE.MAIN = "up", "up/main"
        os.chdir(self.work)
        self.quiet = contextlib.redirect_stdout(io.StringIO())
        self.quiet.__enter__()

    def tearDown(self):
        self.quiet.__exit__(None, None, None)
        os.chdir(self.saved[0])
        os.environ["PATH"] = self.saved[1]
        del os.environ["KORA_RELEASE_REPO"]
        MODULE.REMOTE, MODULE.MAIN = self.saved[2], self.saved[3]
        self.tmp.cleanup()

    def stub(self, name, body):
        path = self.bin / name
        path.write_text(body)
        path.chmod(path.stat().st_mode | stat.S_IEXEC)

    def test_prepare_opens_a_branch_and_leaves_main_alone(self):
        main_before = git(self.remote, "rev-parse", "main")
        MODULE.prepare_release("v1.0.0", "7", "2026-11-13")
        self.assertEqual(git(self.remote, "rev-parse", "main"), main_before)
        self.assertEqual(git(self.remote, "show", "release/v1.0.0:VERSION"), "1.0.0")
        changelog = git(self.remote, "show", "release/v1.0.0:CHANGELOG.md")
        self.assertIn("## v1.0.0 — 2026-11-13", changelog)
        self.assertIn("- feat(crm): add a thing", changelog)
        self.assertTrue(changelog.rstrip().endswith("## v0.9.0 — 2026-01-01"))
        self.assertEqual(git(self.remote, "log", "-1", "--format=%s", "release/v1.0.0"), "release: v1.0.0")

    def test_prepare_refuses_an_existing_tag_and_a_stale_head(self):
        with self.assertRaisesRegex(MODULE.ReleaseError, "already exists"):
            MODULE.prepare_release("v0.9.0", "", "2026-11-13")
        (self.work / "b.txt").write_text("b")
        git(self.work, "add", ".")
        git(self.work, "commit", "-q", "-m", "feat: local only")
        with self.assertRaisesRegex(MODULE.ReleaseError, "HEAD is not"):
            MODULE.prepare_release("v1.0.0", "", "2026-11-13")

    def merge_release(self, version="1.0.0"):
        (self.work / "VERSION").write_text(version + "\n")
        (self.work / "CHANGELOG.md").write_text(f"## v{version} — 2026-11-13\n")
        git(self.work, "add", ".")
        git(self.work, "commit", "-q", "-m", f"release: v{version}")
        git(self.work, "push", "-q", "up", "main")

    def test_tag_needs_the_version_bump_merged(self):
        with self.assertRaisesRegex(MODULE.ReleaseError, "VERSION on up/main is not 1.0.0"):
            MODULE.tag_release("v1.0.0")
        self.assertEqual(git(self.remote, "tag", "-l", "v1.0.0"), "")

    def test_tag_waits_for_green_ci_then_tags_the_merge_commit(self):
        self.merge_release()
        os.environ["STUB_JOBS"] = json.dumps([{"name": "Go", "conclusion": "failure"}, {"name": "Lint", "conclusion": "success"}])
        with self.assertRaisesRegex(MODULE.ReleaseError, "'Go' is failure"):
            MODULE.tag_release("v1.0.0")
        self.assertEqual(git(self.remote, "tag", "-l", "v1.0.0"), "")
        os.environ["STUB_JOBS"] = json.dumps([{"name": "Go", "conclusion": "success"}, {"name": "Lint", "conclusion": "success"}])
        MODULE.tag_release("v1.0.0")
        self.assertEqual(git(self.remote, "rev-list", "-n1", "v1.0.0"), git(self.remote, "rev-parse", "main"))
        with self.assertRaisesRegex(MODULE.ReleaseError, "already exists"):
            MODULE.tag_release("v1.0.0")

    def test_tag_refuses_while_ci_is_still_running(self):
        self.merge_release()
        os.environ["STUB_RUN"] = "123 in_progress"
        try:
            with self.assertRaisesRegex(MODULE.ReleaseError, "has not finished"):
                MODULE.tag_release("v1.0.0")
        finally:
            del os.environ["STUB_RUN"]


if __name__ == "__main__":
    unittest.main()
