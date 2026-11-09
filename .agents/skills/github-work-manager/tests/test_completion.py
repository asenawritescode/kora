import importlib.util
import unittest
from pathlib import Path

SCRIPT = Path(__file__).parents[1] / "scripts" / "evidence.py"
spec = importlib.util.spec_from_file_location("evidence", SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def ledger(**updates):
    value = {"issue": "owner/repo#1", "base_revision": "base", "integration_branch": "main",
             "pr": "https://github.com/owner/repo/pull/1", "head_revision": "head",
             "criteria": [{"id": "AC-1", "status": "PASS", "evidence": "focused test"}],
             "checks": [{"command": "pytest", "outcome": "PASS"}], "review_status": "clear", "disposition": "PR_READY"}
    value.update(updates)
    return value


class CompletionTests(unittest.TestCase):
    def test_missing_ci_is_not_completion_evidence(self):
        self.assertIn("UNAVAILABLE", module.render(ledger(checks=[{"command": "CI", "outcome": "UNAVAILABLE"}])))

    def test_failed_acceptance_criterion_is_recorded(self):
        self.assertIn("AC-1: FAIL", module.render(ledger(criteria=[{"id": "AC-1", "status": "FAIL", "evidence": "assertion"}])))

    def test_pr_created_but_not_merged_cannot_be_done(self):
        with self.assertRaises(ValueError):
            module.render(ledger(disposition="DONE"))

    def test_merged_pr_with_missing_required_test_cannot_be_done(self):
        data = ledger(disposition="DONE", merge_sha="abc", checks=[{"command": "required integration", "outcome": "UNAVAILABLE"}])
        with self.assertRaises(ValueError):
            module.render(data)

    def test_completed_verified_task_requires_merge_and_all_pass(self):
        data = ledger(disposition="DONE", merge_sha="abc", evidence_url="https://github.com/owner/repo/issues/1#issuecomment-2")
        self.assertIn("Disposition: DONE", module.render(data))

    def test_oversized_task_is_not_selected(self):
        status_script = Path(__file__).parents[1] / "scripts" / "issue-status.py"
        spec2 = importlib.util.spec_from_file_location("issue_status_for_completion", status_script)
        selector = importlib.util.module_from_spec(spec2)
        spec2.loader.exec_module(selector)
        snapshot = {"metadata_available": True, "issues": [{"state": "open", "kind": "task", "oversized": True}]}
        self.assertEqual(selector.select(snapshot)["status"], "NO_SELECTION")

    def test_same_pr_correction_and_out_of_scope_feedback_classification(self):
        self.assertEqual(module.feedback_class("correctness defect"), "in_scope")
        self.assertEqual(module.feedback_class("enhancement/new requirement"), "out_of_scope")

    def test_bad_github_state_is_unknown(self):
        with self.assertRaises(ValueError):
            module.validate(ledger(disposition="DONE", merge_sha="abc", checks=[{"command": "ci", "outcome": "UNKNOWN"}]))


if __name__ == "__main__":
    unittest.main()
