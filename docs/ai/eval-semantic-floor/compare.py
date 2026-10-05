#!/usr/bin/env python3
"""Compare default-mode metadata captures against the adjacent frozen manifest.

Exit 0: no observed expected hit lost; 1: recall loss; 2: invalid input.
Only the first ten response rows count, before Slack thread folding.
"""
import argparse
import hashlib
import json
import math
from pathlib import Path
import sys


PROBLEM_IDS = (
    "slack:C09H1QMK882:1790058121.649719",
    "slack:C09TRPZRE4B:1783594503.900139",
    "slack:C05RNSE8TBR:1790774008.666339",
    "slack:C04HZ6JB83V:1790907716.124109",
    "slack:C0BRJGC92KA:1790246293.272499",
)
ROW_FIELDS = {"node_id", "type", "rank", "score_breakdown", "thread_key",
              "body_length", "contains_query_word"}


class InvalidCapture(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise InvalidCapture(message)


def object_fields(value, fields, context):
    require(isinstance(value, dict) and set(value) == fields,
            context + ": invalid metadata fields")


def positive_integer(value):
    return type(value) is int and value > 0


def validate_capture(capture, manifest, digest):
    object_fields(capture, {"manifest_sha256", "label", "mode", "timestamp", "queries"}, "capture")
    require(capture["manifest_sha256"] == digest, "manifest SHA does not match the frozen manifest")
    require(capture["mode"] == manifest["mode"], "capture mode differs from manifest")
    for field in ("label", "timestamp"):
        require(isinstance(capture[field], str) and capture[field], "missing " + field)
    require(isinstance(capture["queries"], list), "queries must be an array")
    expected = {query["id"]: query for query in manifest["queries"]}
    queries = {}
    for query in capture["queries"]:
        object_fields(query, {"query", "params", "response", "error"}, "query")
        query_id = query["query"]
        require(isinstance(query_id, str) and query_id in expected, "unknown query id")
        require(query_id not in queries, "duplicate query: " + query_id)
        require(query["params"] == expected[query_id]["params"], "incorrect params: " + query_id)
        require(query["error"] is None, "recorded error: " + query_id)
        object_fields(query["response"], {"results"}, "response")
        rows = query["response"]["results"]
        require(isinstance(rows, list), "results must be an array: " + query_id)
        for row in rows:
            object_fields(row, ROW_FIELDS, "result")
            for field in ("node_id", "type", "thread_key"):
                require(isinstance(row[field], str) and row[field], "invalid result " + field)
            require(positive_integer(row["rank"]), "invalid result rank")
            require(type(row["body_length"]) is int and row["body_length"] >= 0,
                    "invalid body length")
            require(type(row["contains_query_word"]) is bool, "invalid query-word flag")
            node = row["node_id"]
            if node.startswith("slack:"):
                parts = node.split(":", 2)
                key = row["thread_key"].split(":")
                require(len(parts) == 3 and len(key) == 2 and all(key)
                        and key[0] == parts[1], "invalid Slack thread key")
            else:
                require(row["thread_key"] == node, "non-Slack thread key differs from node id")
            breakdown = row["score_breakdown"]
            object_fields(breakdown, {"sem", "ranks"}, "score breakdown")
            sem = breakdown["sem"]
            require(type(sem) in (int, float) and math.isfinite(sem), "invalid semantic score")
            ranks = breakdown["ranks"]
            require(isinstance(ranks, dict) and all(
                isinstance(arm, str) and arm and positive_integer(rank)
                for arm, rank in ranks.items()), "invalid rank arms")
        queries[query_id] = query
    require(set(queries) == set(expected), "missing manifest query")
    return queries


def thread_mapping(*captures):
    mapping = {}
    for capture in captures:
        for query in capture.values():
            for row in query["response"]["results"]:
                node, key = row["node_id"], row["thread_key"]
                require(node not in mapping or mapping[node] == key,
                        "inconsistent thread mapping")
                mapping[node] = key
    return mapping


def expected_key(node, mapping):
    return mapping.get(node, node.removeprefix("slack:"))


def corrected_noise(row):
    return (set(row["score_breakdown"]["ranks"]) == {"semantic"}
            and row["body_length"] < 20 and not row["contains_query_word"])


def presence(rows, node):
    observed = [row for row in rows if row["node_id"] == node]
    if not observed:
        return "absent"
    return "present " + json.dumps([
        {"rank": row["rank"], "ranks": row["score_breakdown"]["ranks"]}
        for row in observed], sort_keys=True)


def compare_captures(manifest, before, after, stdout):
    mapping = thread_mapping(before, after)
    lost_any = False
    for query in manifest["queries"]:
        query_id = query["id"]
        a_rows = before[query_id]["response"]["results"]
        b_rows = after[query_id]["response"]["results"]
        if query["expected_ids"]:
            a_keys = {row["thread_key"] for row in a_rows[:10]}
            b_keys = {row["thread_key"] for row in b_rows[:10]}
            a_found = [node for node in query["expected_ids"] if expected_key(node, mapping) in a_keys]
            b_found = [node for node in query["expected_ids"] if expected_key(node, mapping) in b_keys]
            lost = [node for node in a_found if node not in b_found]
            lost_any |= bool(lost)
            print(query_id + ": " + json.dumps({"a_found": a_found, "b_found": b_found, "lost": lost}),
                  file=stdout)
        else:
            # Problem nodes are always reported, but enter corrected N only if
            # they satisfy the same metadata predicate as every other probe hit.
            noise_nodes = set(PROBLEM_IDS)
            noise_nodes.update(row["node_id"] for row in a_rows + b_rows if corrected_noise(row))
            for node in sorted(noise_nodes):
                print(f"{query_id} noise {node}: a={presence(a_rows, node)}; "
                      f"b={presence(b_rows, node)}", file=stdout)
    return int(lost_any)


def main(argv=None, *, stdout=None, stderr=None):
    stdout = sys.stdout if stdout is None else stdout
    stderr = sys.stderr if stderr is None else stderr
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("a", type=Path)
    parser.add_argument("b", type=Path)
    args = parser.parse_args(argv)
    try:
        raw = Path(__file__).with_name("manifest.json").read_bytes()
        manifest = json.loads(raw)
        digest = hashlib.sha256(raw).hexdigest()
        before = validate_capture(json.loads(args.a.read_bytes()), manifest, digest)
        after = validate_capture(json.loads(args.b.read_bytes()), manifest, digest)
        return compare_captures(manifest, before, after, stdout)
    except (OSError, ValueError, TypeError, KeyError) as error:
        # Never echo input values (recorded errors may themselves contain text).
        message = str(error) if isinstance(error, InvalidCapture) else "unreadable or malformed capture/manifest"
        print("Invalid comparison: " + message, file=stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
