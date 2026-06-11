package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Observability tools: app_probe + service_probe. Both run a tiny Python
// program INSIDE the sandbox via the shell exec endpoint, so the probe
// originates from the same network namespace the app and its bound
// services live in — localhost is the app, and a bound service's
// host:port is reachable over its tunnel. Python (not curl/nc) because
// it's guaranteed present (the code interpreter is Python) and gives us
// clean structured errors instead of shell exit-code guessing.

func registerProbeTools(server *mcp.Server, c *Client) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "app_probe",
		Description: "Make ONE structured HTTP request to your running app inside the sandbox and get back status code, latency, content-type, and a body snippet — the reliable 'is my app actually serving?' check. " +
			"Probes http://localhost:<port><path> from INSIDE the sandbox (no tunnel, no auth sidecar in the way). " +
			"USE THIS instead of `command_run \"curl ...\"` — structured output, no shell-quoting, and a refused connection comes back as a clean {ok:false, error, hint} that names what to check next (project_list / project_logs). " +
			"After project_start, prefer port_wait (for LISTEN) then app_probe (for a real 200) over eyeballing logs.",
	}, appProbe(c))

	mcp.AddTool(server, &mcp.Tool{
		Name: "service_probe",
		Description: "Check that a bound service (postgres, redis, mysql, mongodb, …) is actually REACHABLE from the sandbox — a TCP connect to its host:port, timed. " +
			"Pass `env_var` (the connection-URL env var the binding exposes, e.g. DATABASE_URL / REDIS_URL — service_bind told you the name) and it parses host+port out of the URL for you. Or pass `host`+`port` explicitly. " +
			"Returns {reachable, host, port, latency_ms, error}. USE to disambiguate 'my app can't connect' BEFORE digging through app logs — tells you whether the problem is the network/binding or your code.",
	}, serviceProbe(c))
}

type appProbeArgs struct {
	SandboxURL     string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the most recent sandbox_create in this MCP session."`
	Port           int    `json:"port" jsonschema:"the TCP port your app listens on inside the sandbox (the one project_list reports under ports[])"`
	Path           string `json:"path,omitempty" jsonschema:"request path, default '/'. Include a leading slash (e.g. '/api/health')."`
	Method         string `json:"method,omitempty" jsonschema:"HTTP method, default GET. GET/HEAD/POST/PUT/DELETE."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"per-probe timeout in seconds, default 10"`
}

func appProbe(c *Client) mcp.ToolHandlerFor[appProbeArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a appProbeArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" || a.Port == 0 {
			return errResult("sandbox_url and port are required"), nil, nil
		}
		path := a.Path
		if path == "" {
			path = "/"
		} else if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		method := strings.ToUpper(a.Method)
		if method == "" {
			method = "GET"
		}
		timeout := a.TimeoutSeconds
		if timeout <= 0 {
			timeout = 10
		}

		// Python json-encodes the inputs so the source is injection-safe
		// regardless of path/method content. maxBody caps the snippet at
		// 2000 bytes so a large HTML page doesn't flood context.
		pyArgs, _ := json.Marshal(map[string]any{
			"port": a.Port, "path": path, "method": method, "timeout": timeout, "maxbody": 2000,
		})
		py := fmt.Sprintf(`import json,time,base64,urllib.request,urllib.error
A=json.loads(base64.b64decode('%s'))
url="http://localhost:%%d%%s"%%(A["port"],A["path"])
t0=time.time()
def ms():return int((time.time()-t0)*1000)
try:
    req=urllib.request.Request(url,method=A["method"])
    with urllib.request.urlopen(req,timeout=A["timeout"]) as r:
        body=r.read(A["maxbody"])
        print(json.dumps({"ok":True,"status_code":r.status,"time_ms":ms(),"content_type":r.headers.get("Content-Type",""),"body_snippet":body.decode("utf-8","replace")}))
except urllib.error.HTTPError as e:
    body=e.read(A["maxbody"])
    print(json.dumps({"ok":True,"status_code":e.code,"time_ms":ms(),"content_type":e.headers.get("Content-Type","") if e.headers else "","body_snippet":body.decode("utf-8","replace")}))
except Exception as e:
    print(json.dumps({"ok":False,"time_ms":ms(),"error":str(e)}))
`, base64.StdEncoding.EncodeToString(pyArgs))

		res, err := runPython(ctx, c, a.SandboxURL, py, timeout+5)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		// Surface a next-action hint when the connection failed — this is
		// the moment the model needs to know to check the process, not
		// retry the probe blindly.
		if ok, _ := res["ok"].(bool); !ok {
			res["hint"] = fmt.Sprintf("nothing answered on localhost:%d%s. Check the app is up: project_list (is it running, does ports[] include %d?), then project_logs name=<project> grep='error|panic|traceback'.", a.Port, path, a.Port)
		}
		return jsonResult(res), res, nil
	}
}

type serviceProbeArgs struct {
	SandboxURL     string `json:"sandbox_url,omitempty" jsonschema:"http(s) URL of the sandbox runtime. OPTIONAL — defaults to the most recent sandbox_create in this MCP session."`
	EnvVar         string `json:"env_var,omitempty" jsonschema:"name of the connection-URL env var to parse for host+port (e.g. DATABASE_URL, REDIS_URL, MONGODB_URL). The value never leaves the sandbox. Provide this OR host+port."`
	Host           string `json:"host,omitempty" jsonschema:"explicit host to connect to (alternative to env_var)"`
	Port           int    `json:"port,omitempty" jsonschema:"explicit port (used with host)"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"connect timeout in seconds, default 5"`
}

func serviceProbe(c *Client) mcp.ToolHandlerFor[serviceProbeArgs, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, a serviceProbeArgs) (*mcp.CallToolResult, any, error) {
		if a.SandboxURL == "" {
			a.SandboxURL = c.LastSandboxURL()
		}
		if a.SandboxURL == "" {
			return errResult("sandbox_url is required"), nil, nil
		}
		if a.EnvVar == "" && (a.Host == "" || a.Port == 0) {
			return errResult("provide env_var, or both host and port"), nil, nil
		}
		timeout := a.TimeoutSeconds
		if timeout <= 0 {
			timeout = 5
		}
		pyArgs, _ := json.Marshal(map[string]any{
			"env_var": a.EnvVar, "host": a.Host, "port": a.Port, "timeout": timeout,
		})
		// The Python resolves host/port (parsing the env-var URL when
		// given), then does a plain TCP connect. Scheme→default-port map
		// covers the services we bind. Reading os.environ keeps the
		// secret value inside the sandbox — only host/port/result return.
		py := fmt.Sprintf(`import json,os,socket,time,base64
from urllib.parse import urlparse
A=json.loads(base64.b64decode('%s'))
DEF={"postgres":5432,"postgresql":5432,"mysql":3306,"redis":6379,"rediss":6379,"mongodb":27017,"mongodb+srv":27017,"amqp":5672,"http":80,"https":443}
host=A["host"];port=A["port"];scheme=""
if A["env_var"]:
    raw=os.environ.get(A["env_var"],"")
    if not raw:
        print(json.dumps({"reachable":False,"error":"env var %%s is not set in the sandbox"%%A["env_var"]}));raise SystemExit
    u=urlparse(raw);scheme=u.scheme
    host=u.hostname or host
    port=u.port or DEF.get(u.scheme,0)
if not host or not port:
    print(json.dumps({"reachable":False,"host":host,"port":port,"error":"could not determine host+port"}));raise SystemExit
t0=time.time()
try:
    s=socket.create_connection((host,int(port)),timeout=A["timeout"]);s.close()
    print(json.dumps({"reachable":True,"host":host,"port":int(port),"scheme":scheme,"latency_ms":int((time.time()-t0)*1000)}))
except Exception as e:
    print(json.dumps({"reachable":False,"host":host,"port":int(port),"scheme":scheme,"latency_ms":int((time.time()-t0)*1000),"error":str(e)}))
`, base64.StdEncoding.EncodeToString(pyArgs))

		res, err := runPython(ctx, c, a.SandboxURL, py, timeout+5)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		if reachable, _ := res["reachable"].(bool); !reachable {
			res["hint"] = "service is not reachable. Confirm it's bound (service_list / the bindings array from sandbox_create), that the env var name is right, and that the operator wired credentials. A binding present but unreachable usually means the cluster service is down or the tunnel isn't up."
		}
		return jsonResult(res), res, nil
	}
}

// runPython runs a Python program inside the sandbox via the shell exec
// endpoint and decodes the single JSON object the program prints. The
// source is sent base64-wrapped so arbitrary quotes/newlines in the
// program never collide with shell quoting. We parse the LAST non-empty
// line as JSON so an incidental warning on an earlier line doesn't break
// decoding.
func runPython(ctx context.Context, c *Client, sandboxURL, src string, execTimeout int) (map[string]any, error) {
	b64 := base64.StdEncoding.EncodeToString([]byte(src))
	cmd := fmt.Sprintf(`python3 -c "import base64;exec(base64.b64decode('%s'))"`, b64)
	res, err := c.Exec(ctx, sandboxURL, ExecRequest{Command: cmd, Timeout: float64(execTimeout)})
	if err != nil {
		return nil, err
	}
	out := strings.TrimSpace(res.Output)
	if out == "" {
		return nil, fmt.Errorf("probe produced no output (exit_code=%d)", res.ExitCode)
	}
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" {
			continue
		}
		var m map[string]any
		if jerr := json.Unmarshal([]byte(ln), &m); jerr == nil {
			return m, nil
		}
		break
	}
	// Couldn't parse — hand back the raw output so the model still sees
	// what happened (e.g. a python traceback from a missing interpreter).
	return nil, fmt.Errorf("probe output was not JSON: %s", truncate(out, 800))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
