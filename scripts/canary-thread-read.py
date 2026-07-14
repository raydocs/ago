#!/usr/bin/env python3
"""One bounded real GLM 5.2 Read Thread canary. Intentionally one model call."""
import json, os, select, subprocess, sys, tempfile, time

class MCP:
    def __init__(self, env):
        root=os.path.realpath(os.path.join(os.path.dirname(__file__),".."))
        self.p=subprocess.Popen([os.path.expanduser("~/.local/bin/claudex-flow"),"mcp"],cwd=root,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,bufsize=1,env=env)
    def send(self,v):
        self.p.stdin.write(json.dumps(v,separators=(",",":"))+"\n"); self.p.stdin.flush()
    def recv(self,rid,timeout=180):
        end=time.time()+timeout
        while time.time()<end:
            ready,_,_=select.select([self.p.stdout,self.p.stderr],[],[],end-time.time())
            for stream in ready:
                line=stream.readline()
                if not line: continue
                if stream is self.p.stderr: print(line.rstrip(),file=sys.stderr); continue
                value=json.loads(line)
                if value.get("id")==rid: return value
        raise TimeoutError(rid)
    def close(self):
        self.p.terminate(); self.p.wait(timeout=2)

def main():
    temp=tempfile.TemporaryDirectory(prefix="claudex-read-thread-canary-")
    project=os.path.join(temp.name,"transcripts","project"); os.makedirs(project)
    thread_id="read-thread-canary-v12"
    rows=[
      {"type":"user","uuid":"u-secret","sessionId":thread_id,"timestamp":"2026-07-13T00:00:00Z","message":{"role":"user","content":"api_key=00000000000000000000000000000000.TESTFIXTURE00000000 Build the parser."}},
      {"type":"user","uuid":"u-compact","sessionId":thread_id,"timestamp":"2026-07-13T00:01:00Z","isCompactSummary":True,"message":{"role":"user","content":"Summary: parser work was completed; inspect original events for the exact verifier."}},
      {"type":"assistant","uuid":"a-proof","sessionId":thread_id,"timestamp":"2026-07-13T00:02:00Z","message":{"role":"assistant","model":"gpt-5.6-sol","content":[{"type":"text","text":"The exact verifier was go test ./internal/parser and it passed."}]}}
    ]
    with open(os.path.join(project,thread_id+".jsonl"),"w") as f:
        for row in rows: f.write(json.dumps(row,separators=(",",":"))+"\n")
    sync=os.path.join(temp.name,"thread-sync.json")
    with open(sync,"w") as f: json.dump({"enabled":False},f)
    env=os.environ.copy(); env.update({
      "CLAUDEX_TRANSCRIPT_ROOT":os.path.join(temp.name,"transcripts"),
      "CLAUDEX_ROUTE_LEDGER_PATH":os.path.join(temp.name,"route-outcomes.jsonl"),
      "CLAUDEX_THREAD_SYNC_CONFIG":sync,
      "CLAUDEX_THREAD_USAGE_STATE":os.path.join(temp.name,"usage-state.json"),
      "CLAUDEX_THREAD_GRAPH_STATE":os.path.join(temp.name,"graph-state.json"),
      "CLAUDEX_SESSION_BINDING_DIR":os.path.join(temp.name,"session-bindings")
    })
    m=MCP(env)
    try:
      m.send({"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"read-thread-canary","version":"1"}}})
      init=m.recv(1,5)["result"]; m.send({"jsonrpc":"2.0","method":"notifications/initialized","params":{}})
      m.send({"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"route_task","arguments":{"objective":"Read prior Thread and extract the exact verifier.","kind":"read-thread"}}})
      route=m.recv(2,5)["result"]["structuredContent"]
      if route["selected_lane"]["tool"]!="read_thread": raise RuntimeError(route)
      m.send({"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_thread","arguments":{"route_id":route["route_id"],"thread_id":thread_id,"question":"What exact verifier was run, and did it pass?","max_source_bytes":16384}}})
      response=m.recv(3,180); result=response.get("result",{})
      if result.get("isError"): raise RuntimeError(json.dumps(result,ensure_ascii=False))
      out=result["structuredContent"]; ident=out["identity"]
      encoded=json.dumps(out,ensure_ascii=False)
      if ident["requested_model"]!="glm-5.2" or ident["model_verification"]!="verified": raise RuntimeError(ident)
      if "go test ./internal/parser" not in encoded or "505ba8" in encoded: raise RuntimeError("missing proof or secret leak")
      if not out.get("session_id") or out["usage"].get("output_tokens",0)<=0: raise RuntimeError("missing session/usage")
      m.send({"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"record_route_outcome","arguments":{"route_id":route["route_id"],"status":"accepted","verification":"GLM returned the exact original-event verifier with thread:// source; secret scan passed.","human_correction":"none","residual_risk":"none for bounded synthetic canary"}}})
      outcome=m.recv(4,5)["result"]["structuredContent"]
      print(json.dumps({"status":"PASS","server":init["serverInfo"],"route_id":route["route_id"],"route_action":route["action"],"identity":ident,"session_id":out["session_id"],"usage":out["usage"],"duration_ms":out["duration_ms"],"thread_source":out["thread_source"],"report":out["report"],"route_diagnostics":outcome["diagnostics"],"route_ledger":outcome["ledger_status"]},ensure_ascii=False,indent=2))
    finally:
      m.close(); temp.cleanup()

if __name__=="__main__": main()
