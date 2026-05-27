# Recipe — scaffold a LangGraph agent

Use this when the user asks for "an agent", "a langgraph agent",
"a tool-using assistant", "a chatbot that can …", or any variant
involving an LLM that calls tools and keeps state.

The Anthropic API key is already in the sandbox: `ANTHROPIC_API_KEY` is
exported in every login shell by `/etc/profile.d/sandbox-creds.sh`
(sourced from `/etc/sandbox/creds/env`). No setup needed.

Default model: **`claude-sonnet-4-5`** unless the user specifies
otherwise. Don't hardcode it in 10 places — pin it in `agent/llm.py`.

## ⚠️ READ THIS FIRST — scaffold BEFORE you explore

The #1 way this recipe goes wrong is: the model spawns a Jupyter
kernel with `code_context_create` to "look at the data first", does
all the analytics inline, and never writes a single source file. At
the end there's no `agent/`, no `.sandbox-project.json`, no project
the user can keep — just an in-memory kernel that dies with the
sandbox.

DO NOT DO THAT. Rules:

1. The **FIRST `file_write` in this task** is the project skeleton
   (manifest + empty `agent/__init__.py`). Even if it has placeholder
   contents. This commits you to the project being a project.
2. Use `code_context_create` / `code_exec` for **ad-hoc data
   exploration ONLY** — `SHOW CATALOGS`, `DESCRIBE`, a few `SELECT …
   LIMIT 50`, a histogram to see distributions. Each call should be
   <30 lines of throwaway code.
3. The moment you're writing a function you intend to KEEP, stop the
   kernel use and `file_write` it into `agent/`. The kernel is a
   scratch buffer, not your codebase.
4. When you finish, `project_list` MUST show your agent running. If
   the only artifact you produced is a Jupyter context plus some
   PNGs, you built nothing and you need to start over.

### Quoting `pip install` versions

`pip install langgraph>=0.2.50` runs in bash, which sees `>=0.2.50` as
output redirection and creates an empty file named `=0.2.50` in the
current directory. ALWAYS quote pinned versions:

```bash
pip install "langgraph>=0.2.50" "langchain-anthropic>=0.3" "pydantic>=2"
```

Better still: write `requirements.txt` first via `file_write`, then
`pip install -r requirements.txt` — no shell quoting needed.

## Layout

```
/workspace/projects/<agent-name>/
├── .sandbox-project.json     # type: "agent"
├── requirements.txt
├── README.md
├── agent/
│   ├── __init__.py           # re-export build_graph()
│   ├── state.py              # TypedDict for graph state (~30 lines)
│   ├── llm.py                # ChatAnthropic factory (~25 lines)
│   ├── prompts.py            # system + per-node prompts as strings (~40 lines)
│   ├── tools.py              # @tool functions (~60-80 lines; split per-domain if >100)
│   ├── nodes.py              # node functions (~80 lines; split if it grows)
│   ├── edges.py              # routing / conditional edges (~40 lines)
│   ├── graph.py              # builds + compiles the StateGraph (~50 lines)
│   └── graph_test.py
├── main.py                   # entrypoint — CLI loop or HTTP server (~50 lines)
└── cli.py                    # optional: typer/click interactive REPL (~40 lines)
```

Every file stays under ~100 lines. If a section above doesn't fit,
split by domain:

- `tools.py` too big → `tools_db.py`, `tools_email.py`, `tools_http.py`,
  with `tools.py` collecting them: `TOOLS = [*db, *email, *http]`.
- `nodes.py` too big → one file per node: `nodes_plan.py`,
  `nodes_act.py`, `nodes_summarize.py`.

## The minimum viable agent — file by file

### `requirements.txt`

```
langgraph>=0.2.50
langchain-anthropic>=0.3
langchain-core>=0.3
pydantic>=2
# add domain libs (httpx, your DB client, boto3, …) as needed
```

### `agent/llm.py`

```python
"""Single source of truth for the model + creds. Import from here."""
import os
from langchain_anthropic import ChatAnthropic

MODEL = "claude-sonnet-4-5"   # update in ONE place when you bump models


def get_llm(model: str | None = None, temperature: float = 0.0) -> ChatAnthropic:
    # ANTHROPIC_API_KEY is already exported by /etc/profile.d/sandbox-creds.sh
    return ChatAnthropic(
        model=model or MODEL,
        temperature=temperature,
        max_tokens=4096,
        api_key=os.environ["ANTHROPIC_API_KEY"],
    )
```

### `agent/state.py`

```python
"""Graph state shape. Add fields only as nodes need them."""
from typing import Annotated, TypedDict
from langgraph.graph.message import add_messages
from langchain_core.messages import AnyMessage


class AgentState(TypedDict):
    # `add_messages` is a reducer — new messages are appended, not overwritten.
    messages: Annotated[list[AnyMessage], add_messages]
    # Add task-specific scratch fields here, e.g.:
    # plan: list[str] | None
    # result: dict | None
```

### `agent/prompts.py`

```python
"""Prompts as data. Keep formatting logic out of here."""

SYSTEM = """\
You are a helpful assistant that … [describe what the agent does].
Use the available tools when the user's request needs real-time data,
side effects, or computation you can't reliably do from memory.
"""

REPLAN_HINT = """\
The last tool call didn't produce what you needed. Reconsider …
"""
```

### `agent/tools.py`

```python
"""Tools exposed to the LLM. Each docstring is the tool's user-facing spec."""
import httpx
from langchain_core.tools import tool


@tool
def fetch_url(url: str) -> str:
    """Fetch the body of an HTTP(S) URL and return it as text.

    Use for: looking up live information from a web page or JSON API.
    Don't use for: writing data, authenticated endpoints (add a dedicated
    tool with the auth wired in).
    """
    r = httpx.get(url, timeout=20.0, follow_redirects=True)
    r.raise_for_status()
    return r.text[:8000]   # cap so the LLM doesn't drown in HTML


TOOLS = [fetch_url]   # exported list the graph binds to the LLM
```

### `agent/nodes.py`

```python
"""Nodes are functions: (state) -> partial state. No I/O outside tools."""
from langchain_core.messages import SystemMessage
from .llm import get_llm
from .prompts import SYSTEM
from .state import AgentState
from .tools import TOOLS


def call_llm(state: AgentState) -> dict:
    llm = get_llm().bind_tools(TOOLS)
    response = llm.invoke([SystemMessage(content=SYSTEM), *state["messages"]])
    return {"messages": [response]}
```

### `agent/edges.py`

```python
"""Conditional routing — pure functions of state."""
from langgraph.prebuilt import tools_condition  # noqa: F401  re-export

# If you need a custom condition, define it here:
def should_continue(state) -> str:
    last = state["messages"][-1]
    if getattr(last, "tool_calls", None):
        return "tools"
    return "end"
```

### `agent/graph.py`

```python
"""Build + compile the StateGraph. One job; keep it small."""
from langgraph.graph import StateGraph, START, END
from langgraph.prebuilt import ToolNode
from .nodes import call_llm
from .state import AgentState
from .tools import TOOLS
from .edges import tools_condition


def build_graph():
    g = StateGraph(AgentState)
    g.add_node("llm", call_llm)
    g.add_node("tools", ToolNode(TOOLS))
    g.add_edge(START, "llm")
    g.add_conditional_edges("llm", tools_condition, {"tools": "tools", END: END})
    g.add_edge("tools", "llm")
    return g.compile()
```

### `agent/__init__.py`

```python
from .graph import build_graph
__all__ = ["build_graph"]
```

### `main.py` — choose ONE of: CLI loop, or HTTP serve

CLI loop:

```python
"""Interactive REPL. `python3 main.py` → chat with the agent in the shell."""
from langchain_core.messages import HumanMessage
from agent import build_graph

def main():
    graph = build_graph()
    state = {"messages": []}
    while True:
        try:
            user = input("> ").strip()
        except (EOFError, KeyboardInterrupt):
            print(); break
        if not user:
            continue
        state["messages"].append(HumanMessage(content=user))
        for event in graph.stream(state, stream_mode="values"):
            state = event
        print(state["messages"][-1].content)

if __name__ == "__main__":
    main()
```

HTTP serve (when the user wants to call the agent over the network —
this is what hooks up to a frontend or any external caller):

```python
"""FastAPI wrapper around the compiled graph."""
from fastapi import FastAPI
from pydantic import BaseModel
from langchain_core.messages import HumanMessage
from agent import build_graph

graph = build_graph()
app = FastAPI(title="agent")

class Turn(BaseModel):
    message: str
    thread_id: str | None = None    # for memory if you add a checkpointer

@app.post("/chat")
def chat(turn: Turn):
    state = {"messages": [HumanMessage(content=turn.message)]}
    final = graph.invoke(state)
    return {"reply": final["messages"][-1].content}

@app.get("/health")
def health():
    return {"ok": True}
```

### `.sandbox-project.json`

```json
{
  "name": "<agent-name>",
  "type": "agent",
  "start_command": ["python3", "-m", "uvicorn", "main:app",
                    "--host", "0.0.0.0", "--port", "8000", "--reload"],
  "auto_restart": true,
  "env": { "PYTHONUNBUFFERED": "1" },
  "health_check": { "port": 8000, "path": "/health" }
}
```

(Drop `health_check` and use the CLI `start_command`
`["python3", "main.py"]` if you went CLI instead of HTTP — but then it
isn't a long-running service and doesn't belong in `project_start`;
hand it to the user as a `command_run` invocation.)

## Recipe — end-to-end

Do these in order. **Don't skip step 0.**

0. **SCAFFOLD FIRST (before any exploration).** Write the skeleton so
   the project directory and manifest exist before you do anything
   else. Concretely, `file_write` these in this order — stubs are fine
   for now:

   - `/workspace/projects/<agent-name>/.sandbox-project.json`
     (manifest — the start_command can point at a `main.py` you
     haven't written yet; it'll get filled in)
   - `/workspace/projects/<agent-name>/README.md` (3 lines: name +
     what the agent does + how to invoke it)
   - `/workspace/projects/<agent-name>/requirements.txt`
   - `/workspace/projects/<agent-name>/agent/__init__.py` (empty)

   After this step, `command_run "ls /workspace/projects/<agent-name>"`
   should list those four entries. The project exists. Now you can
   explore safely without losing track.

1. **Install dependencies.**
   `command_run "cd /workspace/projects/<agent-name> && pip install -r requirements.txt"`.
   Always have `requirements.txt` in the project so future installs
   are reproducible — never `pip install <foo>` ad-hoc.

2. **Explore the problem data** (only AFTER step 0). Open a Jupyter
   context, run `SHOW CATALOGS`, `DESCRIBE`, a few sample queries to
   see what the data looks like. Throwaway code only — at most ~30
   lines per `code_exec`. The moment you find yourself writing a
   function you'd want to reuse, STOP and put it in `agent/tools.py`
   via `file_write`.

3. **Fill in the agent files**, in this dependency order so each
   `file_write` covers a complete unit:
   `state.py` → `llm.py` → `prompts.py` → `tools.py` (real
   implementations now) → `nodes.py` → `edges.py` → `graph.py` →
   `agent/__init__.py` (re-export `build_graph`).

4. **`file_write` `main.py`** — pick CLI or HTTP based on whether the
   user wants to chat with it (CLI) or call it from another service
   (HTTP). Update `.sandbox-project.json`'s `start_command` to match.

5. **`project_start name="<agent-name>"`** (if HTTP). For a CLI agent,
   verify it runs with `command_run "cd /workspace/projects/<name> &&
   python3 main.py"` but don't keep it running.

6. **Verify.** `project_list` shows it running with the bound port
   discovered; `curl http://localhost:8000/health` returns
   `{"ok": true}`. If `project_list` is empty, you skipped step 5 or
   the manifest is wrong — fix it before reporting done.

7. **For HTTP agents:** the agent listens on the container's port 8000.
   Operator-side tunneling exposes it to the user — the sandbox runtime
   doesn't reverse-proxy. Hand the operator the sandbox id + port; they
   route the traffic.

### The exit check

Before you tell the user "the agent is built," run:

```bash
command_run "ls -R /workspace/projects/<agent-name>"
command_run "wc -l /workspace/projects/<agent-name>/agent/*.py /workspace/projects/<agent-name>/main.py"
# and:
project_list(sandbox_url=...)
```

You should see: the full agent/ tree on disk, every file under ~100
lines, the project listed as `running` with a discovered port. If any
of those three checks fail, you haven't finished — keep going.

## Memory / checkpointing

Default to no memory — every turn is a fresh state. When the user wants
the agent to remember across calls, add a checkpointer:

```python
from langgraph.checkpoint.memory import MemorySaver
graph = StateGraph(AgentState).compile(checkpointer=MemorySaver())
# Then pass thread_id in the config:
graph.invoke(state, config={"configurable": {"thread_id": turn.thread_id}})
```

For persistence across restarts use `SqliteSaver` from
`langgraph.checkpoint.sqlite` — drop a `state.db` next to the project.

## Common pitfalls

- **Don't `pip install openai`** — this is an Anthropic agent. Use
  `langchain-anthropic`'s `ChatAnthropic`.
- **Don't bake the model name in node code.** Always go through
  `agent.llm.get_llm()` so you can swap models in one place.
- **Don't put I/O in node functions.** All side effects go through
  `@tool`s — that's what gives the agent the ability to plan, retry,
  and surface errors back to itself.
- **Don't return the full state from a node.** Return only the keys you
  changed (`return {"messages": [response]}` not the whole state) —
  LangGraph's reducers handle the merge.
- **Don't skip `tools_condition`.** Without it the graph either calls
  tools forever or stops before the LLM gets the tool result back.
