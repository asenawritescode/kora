import importlib.util
import unittest
from pathlib import Path

SCRIPT = Path(__file__).parents[1] / "scripts" / "issue-status.py"
spec = importlib.util.spec_from_file_location("issue_status", SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def issue(number, **updates):
    result = {"number": number, "state": "open", "kind": "task", "repo_active": False,
              "active_pr": False, "criteria_sufficient": True, "cross_repo_scope": False,
              "already_implemented": False, "prerequisites_integrated": True,
              "dependencies": [], "priority": 2, "unlocks": 0,
              "created_at": "2026-01-01T00:00:00Z"}
    result.update(updates)
    return result


class SelectionTests(unittest.TestCase):
    def snap(self, issues, **kw):
        return {"metadata_available": True, "global_wip_limit": 1, "active_tasks": 0, "issues": issues, **kw}

    def test_atomic_eligible_selected_using_priority_unlocks_then_age(self):
        old = issue(1)
        more_unlocks = issue(2, unlocks=3)
        high = issue(3, priority=1)
        self.assertEqual(module.select(self.snap([old, more_unlocks, high]))["selected"]["number"], 3)
        self.assertEqual(module.select(self.snap([old, more_unlocks]))["selected"]["number"], 2)
        self.assertEqual(module.select(self.snap([old, issue(4, created_at="2025-01-01T00:00:00Z")]))["selected"]["number"], 4)

    def test_rejects_blocked_dependency_or_unknown_relationships(self):
        self.assertIsNone(module.select(self.snap([issue(1, dependencies=[{"state": "done", "integrated": False}])]))["selected"])
        self.assertIsNone(module.select(self.snap([issue(1, dependencies=None)]))["selected"])

    def test_rejects_active_unmerged_pr(self):
        self.assertIsNone(module.select(self.snap([issue(1, active_pr=True)]))["selected"])

    def test_rejects_already_implemented(self):
        self.assertIsNone(module.select(self.snap([issue(1, already_implemented=True)]))["selected"])

    def test_rejects_cross_repository_scope(self):
        self.assertIsNone(module.select(self.snap([issue(1, cross_repo_scope=True)]))["selected"])

    def test_rejects_insufficient_criteria(self):
        self.assertIsNone(module.select(self.snap([issue(1, criteria_sufficient=False)]))["selected"])

    def test_rejects_oversized_task(self):
        self.assertIsNone(module.select(self.snap([issue(1, oversized=True)]))["selected"])

    def test_wip_limit_prevents_second_task(self):
        result = module.select(self.snap([issue(1)], active_tasks=1))
        self.assertEqual(result["status"], "NO_SELECTION")
        self.assertIsNone(result["selected"])

    def test_unavailable_metadata_is_unknown(self):
        self.assertEqual(module.select({"metadata_available": False})["status"], "UNKNOWN")


if __name__ == "__main__":
    unittest.main()
