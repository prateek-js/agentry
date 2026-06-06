# Projects — managing the one server you build

The server you build inside a sandbox should be a **managed project**,
not a one-off `command_start`. Projects give you auto-restart on file
edits, health checks, port discovery, and a single `project_list` view
of "what's up."

**One sandbox = one project.** `/workspace/projects/` holds exactly one
directory, ever. No "backend + frontend", no companion projects, no
sidecar services. If the user needs a database, queue, cache, or
external API, that's `service_bind` — not another project. The deploy
pipeline ships a single-image app; a second project breaks Deploy.

## When to use which tier — pick the LOWEST that fits

| Use case | Right tool |
|---|---|
| One-shot command that exits (pip install, pytest, curl, git) | `command_run` |
| Throwaway watcher you'll kill before the chat ends | `command_start` |
| The server the user will iterate on across turns (the common case) | `project_start` |

If the user says "build me a dashboard / web app / agent / landing
page / X that I can play with" — that's project territory. Don't
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
  "name": "app",
  "type": "app",                     // "app" | "agent" | "service"
  "start_command": [
    "npm", "run", "dev", "--",
    "--port", "3000",
    "--hostname", "0.0.0.0"
  ],
  "auto_restart": true,              // manager re-spawns on crash
  "env": { "NODE_ENV": "development" },
  "env_file": ".env",                // optional; relative to project dir
  "health_check": {                  // optional
    "port": 3000,                    // REQUIRED when health_check is set
    "path": "/",
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
# Expect: ONE project, status=running, with discovered ports.
```

If `project_list` is empty and you have a running server, you used
`command_start` when you should have used `project_start`. Fix it.

If `project_list` shows TWO projects, you've split the build — that's
a deploy blocker. Pick the one the user actually wants, delete the
other manifest, and re-run.
