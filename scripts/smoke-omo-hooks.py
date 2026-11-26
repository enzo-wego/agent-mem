#!/usr/bin/env python3
"""Run with python3 scripts/smoke-omo-hooks.py [--omo /path/to/omo].

Requires Python 3.9+, macOS sandbox-exec, OmO 5.1.83 and a Go toolchain with a
populated module cache (override with --go-mod-cache). No dependencies are
downloaded. The only provider auth value is a dummy string for the loopback
model. --keep preserves synthetic logs.
"""

import argparse
from collections import Counter
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
from queue import Empty, Queue
import shutil
import signal
import subprocess
import sys
import tempfile
from threading import Thread
import time
from uuid import uuid4


REPO = Path(__file__).resolve().parents[1]
SYSTEM_PATH = "/usr/bin:/bin:/usr/sbin:/sbin"
MODEL = "smoke-model"
FINAL = "OMO_SMOKE_FINAL_ASSISTANT"
EVENTS = ["session-start", "prompt-submit", "post-tool-use", "post-tool-use", "stop"]


def check(condition, message):
    if not condition:
        raise RuntimeError(message)


def start_mock(root, recall):
    """Serve both HTTP protocols on a single isolated, ephemeral loopback port."""
    journal = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            raw = self.rfile.read(int(self.headers["Content-Length"]))
            body = json.loads(raw)
            prior_models = sum(path == "/v1/chat/completions" for path, _ in journal)
            journal.append((self.path, body))
            with (root / "http.jsonl").open("a") as f:
                f.write(json.dumps({"path": self.path, "body": body}) + "\n")
            if self.path == "/v1/chat/completions" and prior_models < 3:
                delta = {"role": "assistant"}
                if prior_models < 2:
                    outcome = ("ok", "fail")[prior_models]
                    delta["tool_calls"] = [{
                        "index": 0, "id": f"call_smoke_{outcome}", "type": "function",
                        "function": {
                            "name": "smoke_probe", "arguments": json.dumps({"outcome": outcome}),
                        },
                    }]
                    reason = "tool_calls"
                else:
                    delta["content"] = FINAL
                    reason = "stop"
                base = {"id": f"smoke-{prior_models}", "object": "chat.completion.chunk",
                        "created": 1, "model": MODEL}
                frames = [
                    {"index": 0, "delta": delta, "finish_reason": None},
                    {"index": 0, "delta": {}, "finish_reason": reason},
                ]
                result = "".join(
                    "data: " + json.dumps({**base, "choices": [frame]}) + "\n\n"
                    for frame in frames
                ) + "data: [DONE]\n\n"
                status, content_type = 200, "text/event-stream"
            elif self.path.removeprefix("/api/hook/") in EVENTS:
                result = recall if self.path == "/api/hook/session-start" else ""
                status, content_type = 200, "text/plain"
            else:
                result, status, content_type = "unexpected fixture request", 400, "text/plain"
            data = result.encode()
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    server = HTTPServer(("127.0.0.1", 0), Handler)
    Thread(target=server.serve_forever, daemon=True).start()
    return server, journal


def run_rpc(command, cwd, env, root, prompt, journal):
    inbox = Queue()
    with (root / "omo.stderr").open("w") as err:
        proc = subprocess.Popen(
            command, cwd=cwd, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=err, text=True, start_new_session=True,
        )

        def collect():
            with (root / "rpc.jsonl").open("w") as out:
                for line in proc.stdout:
                    out.write(line)
                    out.flush()
                    try:
                        inbox.put(json.loads(line))
                    except ValueError:
                        pass
            inbox.put(None)

        reader = Thread(target=collect, daemon=True)
        reader.start()
        seen = []

        def submit(kind, **fields):
            proc.stdin.write(json.dumps({"id": kind, "type": kind, **fields}) + "\n")
            proc.stdin.flush()

        def receive(predicate, description, timeout=30):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                try:
                    event = inbox.get(timeout=max(0, deadline - time.monotonic()))
                except Empty:
                    break
                check(event is not None, f"OmO exited waiting for {description}")
                seen.append(event)
                if predicate(event):
                    return event
            raise RuntimeError(f"RPC timed out waiting for {description}")

        def ack(kind):
            event = receive(
                lambda e: e.get("type") == "response" and e.get("id") == kind, kind
            )
            check(event.get("success") is True, f"RPC {kind} failed: {event}")

        try:
            submit("set_client_info", width=80, capabilities=["interactive_user"])
            ack("set_client_info")
            submit("prompt", message=prompt)
            ack("prompt")
            if not any(e.get("type") == "agent_settled" for e in seen):
                receive(lambda e: e.get("type") == "agent_settled", "agent_settled")

            # The RPC subscriber sees settled before extension handlers finish.
            # Require the stop to reach the worker before requesting shutdown.
            deadline = time.monotonic() + 10
            expected = Counter(f"/api/hook/{event}" for event in EVENTS)
            while True:
                actual = Counter(path for path, _ in journal if path.startswith("/api/hook/"))
                if actual == expected or time.monotonic() >= deadline:
                    break
                time.sleep(0.02)
            check(actual == expected, f"worker events before shutdown: {actual}, expected {expected}")
            submit("prompt", message="/smoke-quit")
            check(proc.wait(timeout=15) == 0, f"OmO shutdown exited {proc.returncode}")
            reader.join(timeout=3)
            check(not reader.is_alive(), "RPC reader did not drain after shutdown")
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=5)
            reader.join(timeout=3)
            proc.stdin.close()
            proc.stdout.close()
    return [e for e in seen if e.get("type") == "tool_execution_end"]


def verify(journal, tool_events, cwd, prompt, recall):
    hooks = [(path.removeprefix("/api/hook/"), body)
             for path, body in journal if path.startswith("/api/hook/")]
    check(Counter(event for event, _ in hooks) == Counter(EVENTS),
          f"unexpected worker events: {hooks}")
    singles = {event: body for event, body in hooks if event != "post-tool-use"}
    start, submitted, stop = (singles[event] for event in ("session-start", "prompt-submit", "stop"))
    sid = start.get("session_id")
    check(isinstance(sid, str) and sid and
          all(body.get("session_id") == sid and body.get("cwd") == str(cwd) for _, body in hooks),
          f"inconsistent session/cwd: {hooks}")
    check(submitted.get("prompt") == prompt, f"unexpected prompt payload: {submitted}")
    tool_payloads = [body for event, body in hooks if event == "post-tool-use"]
    for outcome, expected in [
        ("ok", "OMO_SMOKE_TOOL_SUCCESS"),
        ("fail", "[tool failed]\nOMO_SMOKE_TOOL_FAILURE"),
    ]:
        # Worker consumes tool_response, not a top-level is_error field.
        check(any(p.get("tool_name") == "smoke_probe" and p.get("tool_input") == {"outcome": outcome}
                  and p.get("tool_response") == expected for p in tool_payloads),
              f"wrong worker tool payload for {outcome}: {tool_payloads}")
    check([(e.get("toolName"), e.get("isError")) for e in tool_events] ==
          [("smoke_probe", False), ("smoke_probe", True)], f"wrong native tool events: {tool_events}")
    check(stop.get("last_assistant_message") == FINAL, f"stop lacks final assistant text: {stop}")

    models = [body for path, body in journal if path == "/v1/chat/completions"]
    check(len(models) == 3 and len(journal) == len(hooks) + len(models),
          f"expected exactly three model calls and no other requests; got {[p for p, _ in journal]}")
    first = models[0]["messages"]
    check(recall in json.dumps(first), "recall missing from first actual model request")
    check(any(m.get("role") == "user" and prompt in json.dumps(m) for m in first),
          "synthetic user prompt missing from first model request")
    check(all(body.get("model") == MODEL for body in models), "wrong synthetic model")
    print(f"worker events: {', '.join(event for event, _ in hooks)} (one shared session {sid})")
    print("model requests: 3; first received recall; native tool results: success, failure")
    print("stop: final assistant text present; no duplicate or extra prompt after shutdown")


def smoke(root, omo, go, go_mod_cache):
    cwd, home, agent, bin_dir = [root / p for p in ("project", "home", "agent", "bin")]
    for directory in [cwd, home, agent, bin_dir, root / "tmp"]:
        directory.mkdir()
    env = {
        "PATH": f"{bin_dir}:{SYSTEM_PATH}", "HOME": str(home),
        "TMPDIR": str(root / "tmp"), "LC_ALL": "C",
        **{f"XDG_{name}_HOME": str(home / name.lower())
           for name in ("CACHE", "CONFIG", "DATA", "STATE")},
        "OMO_CODING_AGENT_DIR": str(agent), "PI_OFFLINE": "1", "PI_TELEMETRY": "0",
        "OMO_SMOKE_LIFECYCLE": str(root / "lifecycle"),
    }
    # Read the Go toolchain's dependency/cache paths only; no application env
    # or Go user configuration is passed into the build or runtime.
    go_lookup = {"PATH": SYSTEM_PATH, "HOME": str(Path.home()), "GOENV": "off"}
    cache = subprocess.check_output(
        [go, "env", "-json", "GOMODCACHE", "GOCACHE"], env=go_lookup, text=True, timeout=10
    )
    build_env = {
        **env, **json.loads(cache), "GOENV": "off", "GOTOOLCHAIN": "local", "GOWORK": "off",
        "GOPROXY": "off", "GOSUMDB": "off", "CGO_ENABLED": "0",
    }
    if go_mod_cache:
        build_env["GOMODCACHE"] = str(go_mod_cache.resolve())
    native = bin_dir / "agent-mem"
    subprocess.run([go, "build", "-mod=readonly", "-o", str(native), "./cmd/agent-mem"],
                   env=build_env, cwd=REPO, check=True, timeout=240)
    nonce = uuid4().hex
    prompt = f"Run the isolated synthetic OmO hook probe. Ticket: {nonce}."
    recall = f"OMO_SMOKE_RECALL_{uuid4().hex}"
    mock, journal = start_mock(root, recall)
    try:
        port = mock.server_port
        env["AGENT_MEM_WORKER_PORT"] = str(port)
        fixture = root / "smoke-tools.ts"
        shutil.copyfile(REPO / "testdata/omo/smoke-tools.ts", fixture)
        (agent / "models.json").write_text(json.dumps({"providers": {"fixture": {
            "baseUrl": f"http://127.0.0.1:{port}/v1", "api": "openai-completions",
            "apiKey": "dummy-loopback-value", "models": [{"id": MODEL}],
        }}}))

        # Do not let runtime startup contact other hosts, or even other local TCP services.
        profile = f'''(version 1)
            (allow default)
            (deny network-outbound (remote ip))
            (allow network-outbound (remote ip "localhost:{port}"))'''
        sandbox = ["/usr/bin/sandbox-exec", "-p", profile]

        def command(binary, *args):
            result = subprocess.run(
                [*sandbox, binary, *args], env=env, cwd=cwd, text=True,
                capture_output=True, timeout=25,
            )
            check(result.returncode == 0, f"{binary} {args}: {result.stdout}\n{result.stderr}")
            return result.stdout.strip()

        version = command(omo, "--version")
        check(version.split()[:2] == ["omo", "5.1.83"], f"not the pinned runtime: {version}")
        print(version)
        selected = json.loads(command(omo, "config", "agent-dir", "--json"))
        check(selected["ok"] and selected["dir"] == str(agent) and
              selected["source"] == "OMO_CODING_AGENT_DIR", f"wrong agent dir: {selected}")
        dest = agent / "extensions" / "agent-mem.ts"
        first = command(native, "install-hooks", "omo")
        check(str(dest) in first and dest.is_file(), f"unexpected install target: {first}")
        original = (dest.read_bytes(), dest.stat().st_mtime_ns, dest.stat().st_ino)
        second = command(native, "install-hooks", "omo")
        check("already installed" in second and str(dest) in second and
              (dest.read_bytes(), dest.stat().st_mtime_ns, dest.stat().st_ino) == original and
              list(dest.parent.iterdir()) == [dest], f"non-idempotent reinstall: {second}")
        check(not (home / ".omo/agent/extensions/agent-mem.ts").exists(),
              "installer also wrote to the default OmO path")
        print(f"install: {dest.relative_to(root)} matches runtime agent dir; rerun unchanged")

        # Only the independent fixture is explicitly loaded. The installed hook
        # must be auto-discovered; its path is never passed to the session CLI.
        events = run_rpc(
            [*sandbox, omo, "--mode", "rpc", "--no-session", "--provider", "fixture",
             "--model", MODEL, "--tools", "smoke_probe", "--extension", str(fixture)],
            cwd, env, root, prompt, journal,
        )
        lifecycle = (root / "lifecycle").read_text().splitlines()
        check(lifecycle == ["agent_settled", "session_shutdown"], f"wrong lifecycle: {lifecycle}")
        verify(journal, events, cwd, prompt, recall)
    finally:
        mock.shutdown()
        mock.server_close()


def main():
    args = argparse.ArgumentParser(description=__doc__)
    args.add_argument("--omo", type=Path,
                      default=Path.home() / ".omo/binary-runtime/5.1.83/omo")
    args.add_argument("--go-mod-cache", type=Path, help="use a pre-populated Go module cache")
    args.add_argument("--keep", action="store_true", help="keep synthetic HTTP and RPC logs")
    options = args.parse_args()
    go = shutil.which("go")
    check(go and options.omo.is_file() and Path("/usr/bin/sandbox-exec").is_file(),
          "requires go, the OmO 5.1.83 binary (--omo), and macOS sandbox-exec")
    root = Path(tempfile.mkdtemp(prefix="omo-hooks-smoke-", dir="/tmp")).resolve()
    try:
        smoke(root, options.omo.resolve(), Path(go).resolve(), options.go_mod_cache)
    except Exception as err:
        print(f"FAIL: {err}", file=sys.stderr)
        stderr = root / "omo.stderr"
        if stderr.exists():
            print(stderr.read_text()[-6000:], file=sys.stderr)
        return 1
    finally:
        if options.keep:
            print(f"synthetic artifacts: {root}")
        else:
            shutil.rmtree(root)
    print("PASS: isolated OmO hooks smoke")
    return 0


if __name__ == "__main__":
    sys.exit(main())
