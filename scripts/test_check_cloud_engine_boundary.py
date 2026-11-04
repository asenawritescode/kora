import importlib.util
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("check-cloud-engine-boundary.py")
SPEC = importlib.util.spec_from_file_location("cloud_engine_boundary", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
boundary = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(boundary)


class CloudEngineBoundaryTests(unittest.TestCase):
    def test_accepts_public_seams_and_no_normal_build_implementation_edges(self):
        graph = {
            "edges": [
                {
                    "from_module": "cloud",
                    "kind": "production",
                    "from": package,
                    "to": f"{boundary.ENGINE_PREFIX}{target}",
                }
                for package, target in boundary.EXPECTED_LEGACY_IMPORTS
            ]
            + [
                {
                    "from_module": "cloud",
                    "kind": "production",
                    "from": "github.com/asenawritescode/kora-cloud/internal/app",
                    "to": f"{boundary.ENGINE_PREFIX}{seam}",
                }
                for seam in boundary.PUBLIC_SEAMS
            ],
        }
        self.assertEqual(boundary.check(graph), [])

    def test_rejects_new_implementation_import(self):
        graph = {
            "edges": [
                {
                    "from_module": "cloud",
                    "kind": "production",
                    "from": "github.com/asenawritescode/kora-cloud/internal/app",
                    "to": f"{boundary.ENGINE_PREFIX}workflow",
                },
                *[
                    {
                        "from_module": "cloud",
                        "kind": "production",
                        "from": package,
                        "to": f"{boundary.ENGINE_PREFIX}{target}",
                    }
                    for package, target in boundary.EXPECTED_LEGACY_IMPORTS
                ],
            ]
        }
        self.assertEqual(
            boundary.check(graph),
            [
                "unexpected Cloud production import: github.com/asenawritescode/kora-cloud/internal/app -> Engine workflow"
            ],
        )

    def test_ignores_test_only_implementation_imports(self):
        graph = {
            "edges": [
                {
                    "from_module": "cloud",
                    "kind": "test",
                    "from": "github.com/asenawritescode/kora-cloud/cloud",
                    "to": f"{boundary.ENGINE_PREFIX}site",
                },
                *[
                    {
                        "from_module": "cloud",
                        "kind": "production",
                        "from": package,
                        "to": f"{boundary.ENGINE_PREFIX}{target}",
                    }
                    for package, target in boundary.EXPECTED_LEGACY_IMPORTS
                ],
            ]
        }
        self.assertEqual(boundary.check(graph), [])


if __name__ == "__main__":
    unittest.main()
