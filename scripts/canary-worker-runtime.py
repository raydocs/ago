#!/usr/bin/env python3
"""One bounded real Grok Worker canary. This intentionally makes one model call."""

import json
import os
import select
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class MCP:
    def __init__(self, env=None):
        root = os.path.realpath(os.path.join(os.path.dirname(__file__), ".."))
        self.process = subprocess.Popen(
            [os.path.expanduser("~/.local/bin/claudex-flow"), "mcp"],
            cwd=root,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            env=env,
        )

    def send(self, value):
        self.process.stdin.write(json.dumps(value, separators=(",", ":")) + "\n")
        self.process.stdin.flush()

    def receive(self, request_id, timeout=120):
        deadline = time.time() + timeout
        while time.time() < deadline:
            ready, _, _ = select.select(
                [self.process.stdout, self.process.stderr], [], [], deadline - time.time()
            )
            for stream in ready:
                line = stream.readline()
                if not line:
                    continue
                if stream is self.process.stderr:
                    print(line.rstrip(), file=sys.stderr)
                    continue
                value = json.loads(line)
                if value.get("id") == request_id:
                    return value
        raise TimeoutError(f"MCP response {request_id} timed out")

    def close(self):
        self.process.terminate()
        self.process.wait(timeout=2)


def main():
    received = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            size = int(self.headers.get("Content-Length", "0"))
            received.append(json.loads(self.rfile.read(size)))
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{"ok":true}')

        def log_message(self, _format, *_args):
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()
    temp = tempfile.TemporaryDirectory(prefix="claudex-worker-canary-")
    root_session_id = "canary-root-v10"
    config_path = os.path.join(temp.name, "thread-sync.json")
    with open(config_path, "w", encoding="utf-8") as handle:
        json.dump(
            {
                "enabled": True,
                "endpoint": f"http://127.0.0.1:{server.server_port}/hooks",
                "ingest_token": "canary-ingest",
                "machine_id": "worker-runtime-canary",
                "spool_path": os.path.join(temp.name, "spool.jsonl"),
            },
            handle,
        )
    env = os.environ.copy()
    env.update(
        {
            "CLAUDEX_THREAD_ROOT_SESSION_ID": root_session_id,
            "CLAUDEX_THREAD_SYNC_CONFIG": config_path,
            "CLAUDEX_THREAD_USAGE_STATE": os.path.join(temp.name, "usage-state.json"),
            "CLAUDEX_THREAD_GRAPH_STATE": os.path.join(temp.name, "graph-state.json"),
            "CLAUDEX_SESSION_BINDING_DIR": os.path.join(temp.name, "session-bindings"),
            "CLAUDEX_ROUTE_LEDGER_PATH": os.path.join(temp.name, "route-outcomes.jsonl"),
        }
    )
    mcp = MCP(env)
    try:
        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2025-06-18",
                    "capabilities": {},
                    "clientInfo": {"name": "claudex-worker-runtime-canary", "version": "1"},
                },
            }
        )
        initialized = mcp.receive(1, 5)["result"]
        mcp.send({"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}})

        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {
                    "name": "route_task",
                    "arguments": {
                        "objective": "Read go.mod and report its exact module directive only.",
                        "acceptance_criteria": ["The exact module directive is reported.", "No file changes occur."],
                        "verification_target": "grep '^module ' go.mod",
                        "worker_marginal_contribution": "Prove the pinned Grok Worker runtime so the supervisor only verifies the result.",
                        "independent_slices": 1,
                        "checkability": "objective",
                    },
                },
            }
        )
        route = mcp.receive(2, 5)["result"]["structuredContent"]
        if route["action"] != "bounded_worker":
            raise RuntimeError(f"runtime canary route was not admitted: {route}")

        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {
                    "name": "start_worker",
                    "arguments": {
                        "route_id": route["route_id"],
                        "slice_id": "runtime-canary-v10",
                        "objective": "Read go.mod and report its exact module directive only. Do not edit files.",
                        "marginal_contribution": "Prove the pinned Grok Worker runtime, resolved-model evidence, usage accounting, and parent/child session creation without duplicating implementation work.",
                        "context": "The target is go.mod at the current repository root.",
                        "output_contract": "Return completed status, the exact module directive, go.mod path evidence, empty changed_paths, the read verification, and no capability requests; keep the report under 500 characters.",
                        "done_condition": "The report contains 'module claudexflow', changed_paths is empty, and evidence identifies go.mod.",
                        "deadline_ms": 90000,
                        "write": False,
                    },
                },
            }
        )
        response = mcp.receive(3, 110)
        result = response.get("result", {})
        if result.get("isError"):
            raise RuntimeError(json.dumps(result, ensure_ascii=False))
        worker = result["structuredContent"]
        identity = worker["identity"]
        if worker["state"] != "completed":
            raise RuntimeError(f"worker did not complete: {worker}")
        if identity["requested_model"] != "grok-4.5" or identity["model_verification"] != "verified":
            raise RuntimeError(f"worker model was not verified: {identity}")
        if not worker.get("session_id") or worker["usage"].get("output_tokens", 0) <= 0:
            raise RuntimeError(f"worker session/usage evidence missing: {worker}")
        if worker["report"].get("changed_paths"):
            raise RuntimeError(f"read-only canary changed files: {worker['report']['changed_paths']}")

        deadline = time.time() + 5
        relation = None
        while time.time() < deadline and relation is None:
            for payload in received:
                for event in payload.get("graph_events", []):
                    if (
                        event.get("session_id") == worker["session_id"]
                        and event.get("root_session_id") == root_session_id
                        and event.get("parent_session_id") == root_session_id
                    ):
                        relation = {
                            "root_session_id": event["root_session_id"],
                            "parent_session_id": event["parent_session_id"],
                            "child_session_id": event["session_id"],
                            "event_type": event.get("type"),
                        }
                        break
                if relation is not None:
                    break
            if relation is None:
                time.sleep(0.1)
        if relation is None:
            raise RuntimeError(
                f"no canonical Root/Parent/Child graph event observed; hook payloads={len(received)}"
            )

        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 4,
                "method": "tools/call",
                "params": {
                    "name": "record_route_outcome",
                    "arguments": {
                        "route_id": route["route_id"],
                        "status": "accepted",
                        "verification": "Read evidence reports go.mod:1 module claudexflow; changed_paths is empty.",
                        "human_correction": "none",
                        "residual_risk": "none for this bounded read-only canary",
                    },
                },
            }
        )
        outcome = mcp.receive(4, 5)["result"]["structuredContent"]
        if outcome["state"] != "accepted":
            raise RuntimeError(f"route outcome was not accepted: {outcome}")

        print(
            json.dumps(
                {
                    "status": "PASS",
                    "server": initialized["serverInfo"],
                    "route_action": route["action"],
                    "route_id": route["route_id"],
                    "route_outcome": outcome["outcome"],
                    "route_diagnostics": outcome["diagnostics"],
                    "route_ledger": outcome["ledger_status"],
                    "worker_id": worker["worker_id"],
                    "session_id": worker["session_id"],
                    "state": worker["state"],
                    "identity": identity,
                    "tool_uses": worker.get("tool_uses", {}),
                    "usage": worker["usage"],
                    "duration_ms": worker["duration_ms"],
                    "thread_relation": relation,
                    "report": worker["report"],
                },
                ensure_ascii=False,
                indent=2,
            )
        )
    finally:
        mcp.close()
        server.shutdown()
        server.server_close()
        temp.cleanup()


if __name__ == "__main__":
    main()
