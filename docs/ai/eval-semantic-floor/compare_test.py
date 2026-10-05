"""Metadata-only comparator fixtures; no hub access or message text."""
import copy
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import compare


class CompareTest(unittest.TestCase):
    def setUp(self):
        self.manifest_path = Path(__file__).with_name("manifest.json")
        raw = self.manifest_path.read_bytes()
        self.manifest = json.loads(raw)
        self.capture = {
            "manifest_sha256": hashlib.sha256(raw).hexdigest(),
            "label": "fixture",
            "mode": "default",
            "timestamp": "2026-10-05T00:00:00Z",
            "queries": [
                {"query": q["id"], "params": copy.deepcopy(q["params"]),
                 "response": {"results": []}, "error": None}
                for q in self.manifest["queries"]
            ],
        }
        self.capture["queries"][0]["response"]["results"] = [self.row("jira:PAY-2307")]

    @staticmethod
    def row(node_id, thread_key=None, arms=None, length=30, contains=False):
        return {"node_id": node_id, "type": "slack" if node_id.startswith("slack:") else "jira",
                "rank": 1, "score_breakdown": {"sem": 0.7, "ranks": arms or {"semantic": 1}},
                "thread_key": thread_key or node_id.removeprefix("slack:"),
                "body_length": length, "contains_query_word": contains}

    def run_pair(self, before, after):
        with tempfile.TemporaryDirectory() as directory:
            a, b = Path(directory) / "a.json", Path(directory) / "b.json"
            a.write_text(json.dumps(before))
            b.write_text(json.dumps(after))
            out = io.StringIO()
            status = compare.main([str(a), str(b)], stdout=out, stderr=io.StringIO())
            return status, out.getvalue()

    def test_unchanged(self):
        self.assertEqual(self.run_pair(self.capture, self.capture)[0], 0)

    def test_fold_equivalent_reply(self):
        root = "slack:CUV9EAYGY:1786442428.281449"
        self.capture["queries"][0]["response"]["results"] = [self.row(root)]
        after = copy.deepcopy(self.capture)
        after["queries"][0]["response"]["results"] = [
            self.row("slack:CUV9EAYGY:1790000000.000001", root.removeprefix("slack:"))]
        self.assertEqual(self.run_pair(self.capture, after)[0], 0)

    def test_expected_reply_uses_available_mapping(self):
        expected = "slack:CUV9EAYGY:1786442428.281449"
        root = "CUV9EAYGY:1786000000.000001"
        self.capture["queries"][0]["response"]["results"] = [self.row(expected, root)]
        after = copy.deepcopy(self.capture)
        after["queries"][0]["response"]["results"] = [self.row("slack:" + root, root)]
        self.assertEqual(self.run_pair(self.capture, after)[0], 0)

    def test_lost_expected(self):
        after = copy.deepcopy(self.capture)
        after["queries"][0]["response"]["results"] = []
        status, output = self.run_pair(self.capture, after)
        self.assertEqual(status, 1)
        self.assertIn("jira:PAY-2307", output)

    def test_first_ten_rows_before_folding(self):
        after = copy.deepcopy(self.capture)
        rows = [self.row("slack:C:1", "C:1") for _ in range(10)]
        rows.append(self.row("jira:PAY-2307"))
        after["queries"][0]["response"]["results"] = rows
        self.assertEqual(self.run_pair(self.capture, after)[0], 1)

    def test_missing_query(self):
        after = copy.deepcopy(self.capture)
        after["queries"].pop()
        self.assertEqual(self.run_pair(self.capture, after)[0], 2)

    def test_sha_mismatch(self):
        after = copy.deepcopy(self.capture)
        after["manifest_sha256"] = "0" * 64
        self.assertEqual(self.run_pair(self.capture, after)[0], 2)
        self.assertEqual(self.run_pair(after, after)[0], 2)

    def test_recorded_error(self):
        after = copy.deepcopy(self.capture)
        after["queries"][0]["error"] = "fixture failure"
        self.assertEqual(self.run_pair(self.capture, after)[0], 2)

    def test_duplicate_query(self):
        after = copy.deepcopy(self.capture)
        after["queries"].append(copy.deepcopy(after["queries"][0]))
        self.assertEqual(self.run_pair(self.capture, after)[0], 2)

    def test_wrong_params(self):
        after = copy.deepcopy(self.capture)
        after["queries"][0]["params"]["limit"] = 10
        self.assertEqual(self.run_pair(self.capture, after)[0], 2)

    def test_message_text_rejected(self):
        after = copy.deepcopy(self.capture)
        after["queries"][0]["response"]["results"][0]["body"] = ""
        self.assertEqual(self.run_pair(self.capture, after)[0], 2)

    def test_corrected_noise_and_arm_reporting(self):
        probe = self.capture["queries"][10]
        probe["response"]["results"] = [
            self.row("slack:C:noise", length=7),
            self.row("slack:C:relevant", length=7, contains=True),
            self.row("slack:C:keyword", arms={"keyword": 1}, length=7),
        ]
        after = copy.deepcopy(self.capture)
        after["queries"][10]["response"]["results"] = [
            self.row("slack:C:noise", arms={"keyword": 2}, length=7)]
        status, output = self.run_pair(self.capture, after)
        self.assertEqual(status, 0)
        self.assertIn("slack:C:noise", output)
        self.assertIn('"keyword": 2', output)
        self.assertNotIn("slack:C:relevant", output)
        self.assertNotIn("slack:C:keyword", output)
        for node in compare.PROBLEM_IDS:
            self.assertIn(node, output)


if __name__ == "__main__":
    unittest.main()
