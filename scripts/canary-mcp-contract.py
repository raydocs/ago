#!/usr/bin/env python3
"""Zero-model MCP handshake and admission canary for the installed runtime."""

import json
import os
import select
import subprocess
import sys
import tempfile
import time


EXPECTED_TOOLS = {
    "route_task",
    "record_route_outcome",
    "start_worker",
    "resume_worker",
    "search_external",
    "digest_urls",
    "explore_repository",
    "find_thread",
    "read_thread",
    "consult_native_claude",
    "close_worker",
    "workflow_status",
    "runtime_contract",
}
EXPECTED_FIELDS = [
    "context",
    "deadline_ms",
    "done_condition",
    "marginal_contribution",
    "objective",
    "output_contract",
    "paths",
    "retry_reason",
    "route_id",
    "slice_id",
    "workdir",
    "write",
]
EXPECTED_REQUIRED = {
    "slice_id",
    "objective",
    "marginal_contribution",
    "output_contract",
    "done_condition",
}


class MCP:
    def __init__(self, env=None):
        root = os.path.realpath(os.path.join(os.path.dirname(__file__), ".."))
        binary = os.path.expanduser("~/.local/bin/claudex-flow")
        self.process = subprocess.Popen(
            [binary, "mcp"],
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

    def receive(self, request_id, timeout=5):
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
    temp = tempfile.TemporaryDirectory(prefix="claudex-contract-canary-")
    env = os.environ.copy()
    env["CLAUDEX_ROUTE_LEDGER_PATH"] = os.path.join(temp.name, "route-outcomes.jsonl")
    transcript_root = os.path.join(temp.name, "transcripts")
    project = os.path.join(transcript_root, "-Users-test-project-x")
    os.makedirs(project)
    with open(os.path.join(project, "thread-find.jsonl"), "w") as stream:
        stream.write(
            '{"type":"user","uuid":"u1","sessionId":"thread-find","timestamp":"2026-07-13T00:00:00Z","cwd":"/Users/test/project/x","message":{"role":"user","content":"Implement usage ledger"}}\n'
        )
        stream.write(
            '{"type":"assistant","uuid":"a1","sessionId":"thread-find","timestamp":"2026-07-13T00:01:00Z","cwd":"/Users/test/project/x","message":{"role":"assistant","model":"gpt-5.6-sol","content":[{"type":"tool_use","id":"tool-1","name":"Edit","input":{"file_path":"thread-app/src/usage.ts"}}]}}\n'
        )
    env["CLAUDEX_TRANSCRIPT_ROOT"] = transcript_root
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
                    "clientInfo": {"name": "claudex-v10-canary", "version": "1"},
                },
            }
        )
        initialized = mcp.receive(1)["result"]
        mcp.send(
            {"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}}
        )
        mcp.send(
            {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}}
        )
        tools = mcp.receive(2)["result"]["tools"]
        assert {tool["name"] for tool in tools} == EXPECTED_TOOLS
        start = next(tool for tool in tools if tool["name"] == "start_worker")
        assert sorted(start["inputSchema"]["properties"]) == EXPECTED_FIELDS
        assert set(start["inputSchema"]["required"]) == EXPECTED_REQUIRED

        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 8,
                "method": "tools/call",
                "params": {
                    "name": "route_task",
                    "arguments": {
                        "objective": "Find thread that modified thread-app/src/usage.ts.",
                        "kind": "find-thread",
                    },
                },
            }
        )
        find_route = mcp.receive(8)["result"]["structuredContent"]
        assert find_route["selected_lane"]["tool"] == "find_thread"
        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 9,
                "method": "tools/call",
                "params": {
                    "name": "find_thread",
                    "arguments": {
                        "route_id": find_route["route_id"],
                        "file": "thread-app/src/usage.ts",
                        "project": "x",
                    },
                },
            }
        )
        found = mcp.receive(9)["result"]["structuredContent"]
        assert found["result"]["matches"][0]["thread_id"] == "thread-find"
        assert "read_thread" in found["next_action"]

        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {
                    "name": "route_task",
                    "arguments": {
                        "objective": "Implement an isolated parser and run go test.",
                        "acceptance_criteria": [
                            "Parser behavior matches the frozen fixture."
                        ],
                        "verification_target": "go test ./parser",
                        "worker_marginal_contribution": "Own the isolated parser implementation so the supervisor only verifies it.",
                        "independent_slices": 1,
                        "checkability": "objective",
                    },
                },
            }
        )
        route = mcp.receive(3)["result"]["structuredContent"]
        assert route["route_id"].startswith("route-")
        assert route["action"] == "bounded_worker"
        assert route["selected_lane"]["tool"] == "start_worker"
        assert route["accounting_unit"] == "relative_resource_intensity"
        assert route["durable_default"] is False

        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 6,
                "method": "tools/call",
                "params": {
                    "name": "route_task",
                    "arguments": {
                        "objective": "Research today's current vendor announcement.",
                        "kind": "realtime",
                        "lane_health": [
                            {
                                "tool": "search_external",
                                "status": "unavailable",
                                "failure_class": "auth_configuration",
                                "reason": "zero-model quarantine canary",
                            }
                        ],
                    },
                },
            }
        )
        blocked_route = mcp.receive(6)["result"]["structuredContent"]
        assert blocked_route["action"] == "capability_blocked"
        assert blocked_route["blocked_capability"] == "search_external"
        assert blocked_route["selected_lane"]["model"] == "grok-4.5"

        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 7,
                "method": "tools/call",
                "params": {
                    "name": "record_route_outcome",
                    "arguments": {
                        "route_id": route["route_id"],
                        "status": "abandoned",
                        "human_correction": "none",
                        "residual_risk": "Zero-model contract canary intentionally did not execute the selected Worker.",
                    },
                },
            }
        )
        closed_route = mcp.receive(7)["result"]["structuredContent"]
        assert closed_route["state"] == "abandoned"
        assert closed_route["diagnostics"]["child_model_calls"] == 0
        assert closed_route["diagnostics"]["supervisor_included"] is False
        assert closed_route["ledger_status"] == "persisted"

        # Pass the JSON schema but fail runtime admission. This must consume no
        # child model call, worker start, turn, concurrency slot, or lease.
        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 4,
                "method": "tools/call",
                "params": {
                    "name": "start_worker",
                    "arguments": {
                        "slice_id": "zero-model-admission-canary",
                        "objective": "",
                        "marginal_contribution": "",
                        "output_contract": "",
                        "done_condition": "",
                    },
                },
            }
        )
        rejected = mcp.receive(4)["result"]
        assert rejected.get("isError") is True

        mcp.send(
            {
                "jsonrpc": "2.0",
                "id": 5,
                "method": "tools/call",
                "params": {"name": "workflow_status", "arguments": {}},
            }
        )
        status = mcp.receive(5)["result"]["structuredContent"]
        assert status["contract"]["version"] == "claudex-workflow.v1.3"
        assert status["worker_starts"] == 0
        assert status["worker_turns"] == 0
        assert status["active_runs"] == 0
        assert status["workers"] == []
        assert status["thread_find_calls"] == 1
        assert status["slices"][0]["state"] == "rejected"
        assert any(item["route_id"] == route["route_id"] and item["state"] == "abandoned" for item in status["routes"])
        assert "identity" not in status["slices"][0]
        assert "usage" not in status["slices"][0]

        print(
            json.dumps(
                {
                    "status": "PASS",
                    "protocol": initialized["protocolVersion"],
                    "server": initialized["serverInfo"],
                    "tools": len(tools),
                    "route_action": route["action"],
                    "quarantine_action": blocked_route["action"],
                    "route_lifecycle": closed_route["state"],
                    "route_ledger": closed_route["ledger_status"],
                    "find_thread_matches": len(found["result"]["matches"]),
                    "start_worker_fields": EXPECTED_FIELDS,
                    "admission_model_calls": 0,
                },
                indent=2,
            )
        )
    finally:
        mcp.close()
        temp.cleanup()


if __name__ == "__main__":
    main()
