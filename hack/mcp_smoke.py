#!/usr/bin/env python3
"""Drive `agentry mcp` over stdio and time each step."""
import json, subprocess, sys, time, threading

proc = subprocess.Popen(
    ["/Users/ashwinrajeeva/.local/bin/agentry", "mcp"],
    stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    text=True, bufsize=1,
)

# Drain stderr in the background so we see logs but don't block.
def drain_stderr():
    for line in proc.stderr:
        print(f"[stderr] {line.rstrip()}", file=sys.stderr)
threading.Thread(target=drain_stderr, daemon=True).start()

def call(payload, label):
    t0 = time.time()
    proc.stdin.write(json.dumps(payload) + "\n")
    proc.stdin.flush()
    line = proc.stdout.readline()
    t1 = time.time()
    print(f"=== {label} took {(t1-t0)*1000:.0f} ms ===")
    try:
        resp = json.loads(line)
    except Exception:
        print(f"raw: {line!r}")
        return None
    # Print compact summary
    if "result" in resp:
        r = resp["result"]
        if isinstance(r, dict) and "content" in r:
            for c in r["content"][:3]:
                if c.get("type") == "text":
                    print(c["text"][:400])
        else:
            print(json.dumps(r)[:400])
    elif "error" in resp:
        print(f"ERROR: {resp['error']}")
    return resp

# 1. Initialize
call({"jsonrpc":"2.0","id":1,"method":"initialize","params":{
    "protocolVersion":"2024-11-05",
    "capabilities":{},
    "clientInfo":{"name":"smoke","version":"0"},
}}, "initialize")

# 2. notification — protocol requires we send 'notifications/initialized'
proc.stdin.write(json.dumps({"jsonrpc":"2.0","method":"notifications/initialized"}) + "\n")
proc.stdin.flush()

# 3. sandbox_list (cheap; tests tunnel + bridge + cluster)
call({"jsonrpc":"2.0","id":2,"method":"tools/call","params":{
    "name":"sandbox_list","arguments":{},
}}, "sandbox_list")

# 4. sandbox_create (the failing call)
sid = f"smoke-{int(time.time())}"
call({"jsonrpc":"2.0","id":3,"method":"tools/call","params":{
    "name":"sandbox_create","arguments":{"sandbox_id":sid},
}}, f"sandbox_create({sid})")

# 5. sandbox_list again (should show new sandbox)
call({"jsonrpc":"2.0","id":4,"method":"tools/call","params":{
    "name":"sandbox_list","arguments":{},
}}, "sandbox_list (post-create)")

# 6. sandbox_delete (cleanup)
call({"jsonrpc":"2.0","id":5,"method":"tools/call","params":{
    "name":"sandbox_delete","arguments":{"sandbox_id":sid},
}}, "sandbox_delete")

proc.stdin.close()
proc.wait(timeout=5)
