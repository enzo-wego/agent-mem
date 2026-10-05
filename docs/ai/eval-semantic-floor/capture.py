"""Capture default-mode hub search without accessing credentials or settings."""
import argparse
import datetime
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys

HERE = Path(__file__).resolve().parent
INACTIVE_TEMPORAL = "no time window in query or since/until"


class MCP:
    def __init__(self):
        self.process = subprocess.Popen(
            ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "enzo@payments",
             "/opt/homebrew/bin/docker exec -i agent-mem-worker-1 agent-mem mcp"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            text=True)
        self.sequence = 0

    def send(self, method, params, notification=False):
        self.sequence += 1
        message = {"jsonrpc": "2.0", "method": method, "params": params}
        if not notification:
            message["id"] = self.sequence
        self.process.stdin.write(json.dumps(message) + "\n")
        self.process.stdin.flush()
        if notification:
            return None
        for line in self.process.stdout:
            response = json.loads(line)
            if response.get("id") != self.sequence:
                continue
            if "error" in response:
                raise ValueError(f"{method}: MCP request failed")
            return response["result"]
        raise ValueError(f"{method}: MCP transport closed")

    def call(self, name, arguments):
        result = self.send("tools/call", {"name": name, "arguments": arguments})
        if result.get("isError"):
            raise ValueError(f"{name}: tool call failed")
        if "structuredContent" in result:
            return result["structuredContent"]
        for content in result.get("content", []):
            if content.get("type") == "text":
                return json.loads(content["text"])
        raise ValueError(f"{name}: missing JSON response")

    def close(self):
        self.process.stdin.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.terminate()
            self.process.wait(timeout=5)


def capture(label):
    manifest_bytes = (HERE / "manifest.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    client = MCP()
    records = []
    node_metrics = {}
    slack_ids = set()
    try:
        client.send("initialize", {"protocolVersion": "2024-11-05", "capabilities": {},
                                  "clientInfo": {"name": "semantic-floor-capture", "version": "1"}})
        client.send("notifications/initialized", {}, notification=True)
        for query in manifest["queries"]:
            print(f"Capturing {query['id']}", file=sys.stderr, flush=True)
            response = client.call("graph_search", query["params"])
            for arm, error in (response.get("arm_errors") or {}).items():
                if not (arm == "temporal" and error == INACTIVE_TEMPORAL
                        and not query["params"].get("since")
                        and not query["params"].get("until")):
                    raise ValueError(f"{query['id']}: arm {arm} failed")
            results = response.get("results")
            if not isinstance(results, list):
                raise ValueError(f"{query['id']}: missing results list")
            if query["id"].startswith("Q") and not results:
                raise ValueError(f"{query['id']}: zero eval results")
            safe_results = []
            for rank, row in enumerate(results, 1):
                node_id = row["node_id"]
                if node_id.startswith("slack:"):
                    slack_ids.add(node_id)
                metric_key = (node_id, query["params"]["q"])
                if metric_key not in node_metrics:
                    node = client.call("graph_node", {"id": node_id})
                    body = node.get("body") or ""
                    node_metrics[metric_key] = (len(body), query["params"]["q"].casefold() in body.casefold())
                    del body, node
                length, contains = node_metrics[metric_key]
                breakdown = row["score_breakdown"]
                safe_results.append({
                    "node_id": node_id, "type": row["type"], "rank": rank,
                    "score_breakdown": {"sem": breakdown.get("sem", 0),
                                        "ranks": breakdown.get("ranks") or {}},
                    "body_length": length, "contains_query_word": contains,
                })
            records.append({"query": query["id"], "params": query["params"],
                            "response": {"results": safe_results}, "error": None})
            del response, results
    finally:
        client.close()
    thread_keys = {}
    if slack_ids:
        if any(not re.fullmatch(r"slack:[A-Z0-9]+:[0-9.]+", x) for x in slack_ids):
            raise ValueError("Invalid Slack node id for SQL lookup")
        array = ",".join(sorted(slack_ids))
        sql = "SELECT id, scope, metadata->>'thread_ts' FROM graph.nodes WHERE id = ANY('{" + array + "}')"
        command = '/opt/homebrew/bin/docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -At -c "' + sql + '"'
        result = subprocess.run(["ssh", "-o", "BatchMode=yes", "enzo@payments", command],
                                check=True, capture_output=True, text=True)
        for line in result.stdout.splitlines():
            node_id, scope, thread_ts = line.split("|", 2)
            _, channel, ts = node_id.split(":", 2)
            thread_keys[node_id] = channel + ":" + (thread_ts or ts)
        if set(thread_keys) != slack_ids:
            raise ValueError("Thread-key lookup missing returned Slack nodes")
    for record in records:
        for row in record["response"]["results"]:
            row["thread_key"] = thread_keys.get(row["node_id"], row["node_id"])
    return {"timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
            "label": label, "mode": "default", "queries": records}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--label", required=True, choices=["baseline", "pre", "post", "rollback"])
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        data = capture(args.label)
    except (ValueError, OSError, subprocess.SubprocessError, KeyError) as error:
        print(f"Capture failed: {error}", file=sys.stderr)
        return 1
    timestamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    path = args.output or HERE / f"{args.label}-{timestamp}.json"
    path.write_text(json.dumps(data, indent=2) + "\n")
    print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
