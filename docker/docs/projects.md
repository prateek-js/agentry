# Projects — managing what you build

Most servers you build inside a sandbox should be a **managed
project**, not a one-off `command_start`. Projects give you
auto-restart on file edits, health checks, declared dependencies
between services, and a single `project_list` view of "what's up."

For multi-service apps (backend + frontend, api + worker, …) use
**`depends_on`** between projects and call **`project_start_all`** to
bring the whole tree up. There is no separate "stack" concept.

## When to use which tier — pick the LOWEST that fits

| Use case | Right tool |
|---|---|
| One-shot command that exits (pip install, pytest, curl, git) | `command_run` |
| Throwaway watcher you'll kill before the chat ends | `command_start` |
| Server you'll iterate on across turns (the common case) | `project_start` |
| Multi-service app | one project per service + `project_start_all` |

If the user says "build me a dashboard / FastAPI service / agent / X
that I can play with" — that's almost always project territory. Don't
reach for `command_start` just because the current turn ends with the
server up.

## Project manifest

Path: **`/workspace/projects/<name>/.sandbox-project.json`** (the
project manager also accepts `/workspace/.sandbox-project.json` for
single-service repos at the workspace root). The `<name>` segment must
match the `name` field inside the JSON and the argument to
`project_start`.

Schema:

```jsonc
{
  "name": "backend",
  "type": "service",                 // "app" | "agent" | "service"
  "start_command": [
    "python3", "-m", "uvicorn", "app.main:app",
    "--host", "0.0.0.0", "--port", "8001",
    "--reload"                       // auto-restart on edits
  ],
  "auto_restart": true,              // manager re-spawns on crash
  "depends_on": [],                  // ["backend", "db"] — names of other projects
  "env": { "PYTHONUNBUFFERED": "1" },
  "env_file": ".env",                // optional; relative to project dir
  "health_check": {                  // optional
    "port": 8001,                    // REQUIRED when health_check is set
    "path": "/health",
    "interval": 10,                  // seconds (default 10)
    "timeout":  3,                   // seconds (default 3)
    "retries":  3                    // failures before "unhealthy"
  },
  "resources": {                     // optional
    "max_memory_mb": 1024,
    "max_cpu_percent": 80
  }
}
```

`start_command` is an **argv array**, not a shell string — there's no
implicit shell interpolation. If you need a shell, make the first two
entries `["bash", "-lc", "<the shell command>"]`.

Working directory: the manager runs the process with
`cwd = /workspace/projects/<name>/`. Reference files relative to that.

Ports: the manager auto-discovers every TCP port the process tree
binds via its PGID. You don't pre-allocate; just bind whatever your
code wants. `project_list` reports the bound set as `ports: [...]`.

## Multi-service apps — use `depends_on` + `project_start_all`

When the user wants a backend + frontend (or API + worker, or any
fan-out), give each service its own project manifest and wire the
relationships via `depends_on`. There's no manifest file describing
"the app" — the directory `/workspace/projects/` IS the manifest.

Tree:

```
/workspace/projects/
├── backend/
│   ├── .sandbox-project.json
│   ├── requirements.txt
│   └── app/
│       ├── main.py
│       └── …
└── frontend/
    ├── .sandbox-project.json
    ├── package.json
    └── src/
        └── …
```

`projects/backend/.sandbox-project.json`:

```json
{
  "name": "backend",
  "type": "service",
  "start_command": ["python3", "-m", "uvicorn", "app.main:app",
                    "--host", "0.0.0.0", "--port", "8001", "--reload"],
  "auto_restart": true,
  "env": { "PYTHONUNBUFFERED": "1" },
  "health_check": { "port": 8001, "path": "/health" }
}
```

`projects/frontend/.sandbox-project.json`:

```json
{
  "name": "frontend",
  "type": "app",
  "start_command": ["npm", "run", "dev", "--", "--host", "0.0.0.0", "--port", "5173"],
  "auto_restart": true,
  "depends_on": ["backend"]
}
```

Then:

```
project_start_all(sandbox_url=…)
# manager starts backend first (depends_on chain),
# then frontend; both auto-restart on crash.

project_list(sandbox_url=…)
# returns each project's status, pid, discovered ports[], health.

# To give the user a clickable URL for the frontend: hand the operator
# the sandbox id + port 5173 — their tunneling layer exposes it.
```

Alternatively, `project_start(name="frontend")` cascades through
`depends_on` — calling it on the topmost service brings up the whole
chain. `project_start_all` is just the "everything that's there"
shortcut.

When you `file_write` a new route or component, the `--reload` /
`vite dev` watcher picks it up automatically — no `project_start
restart=true` needed for code edits. Use `restart=true` when env or
config changed.

## Migrating an existing `command_start` to a project

You probably already started a server via `command_start` and want to
upgrade. Do this:

1. `command_interrupt` the background command.
2. `file_write /workspace/projects/<name>/.sandbox-project.json` with
   the schema above. Move the source under
   `/workspace/projects/<name>/` if it isn't there already.
3. `project_start(name="<name>")`.

It's a 3-tool-call migration. Cheaper than continuing to drive a
hand-rolled `command_start` for the rest of the chat.

## Self-check before declaring "the app is up"

```
project_list(sandbox_url=…)
# Expect: every project you intended to run, status=running,
# with discovered ports.
```

If `project_list` is empty and you have a running server, you used
`command_start` when you should have used `project_start`. Fix it.
