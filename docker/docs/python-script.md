# Recipe: long-running Python process

For workers, scrapers, batch jobs, polling loops — any Python
process that runs continuously but doesn't bind a public port.

**Prerequisite: `docs_read("CONTRACT")` — especially rule 3 (bind
services before coding against them) and rule 5 (report platform
problems, don't patch around them).** Observe the running process
with `project_logs` (same tool as every other kind).

## Lifecycle (don't skip)

```
project_create  { name: "<short-name>", kind: "python-script" }
project_start   { name: "<short-name>" }
```

`project_create` writes:
- `/workspace/projects/<name>/.sandbox-project.json`
- `/workspace/projects/<name>/main.py` (stub `print + sleep` loop —
  overwrite)

`project_start` runs `python3 main.py` from the project dir with
`auto_restart: true` on crash.

## Visual surface

None. This recipe is for headless processes. If the user later asks
"can I see what it's doing in a UI", that's a NEW request — switch
to `kind: fastapi` (expose a `/status` endpoint) or `kind: streamlit`
(rebuild as a Streamlit app that polls).

## Layout

```
projects/<name>/
  .sandbox-project.json
  main.py                       (entry; the runtime starts this)
  jobs/                         (per-feature modules per coding-style.md)
  storage/                      (where the script reads/writes; ephemeral
                                  unless you bind a real DB via service_bind)
  requirements.txt              (if you need 3rd-party libs)
```

## When NOT to use python-script

- The user wants the result accessible via HTTP. Use `fastapi`.
- The user wants an interactive UI. Use `streamlit` or `nextjs`.
- The job is one-shot, not continuous. Use `command_run` directly
  — the project manager is for long-running processes, not for
  scripts that exit after one execution.

## Anti-patterns

- ❌ `file_write .sandbox-project.json` — use `project_create`.
- ❌ `command_start "python3 main.py"` — `project_start` gives you
  auto-restart, logs, and crash detection for free.
- ❌ Tight `while True: pass` with no `sleep` — wastes CPU and the
  auto-restart loop kicks in if the process eats too much.
- ❌ Writing state to in-process variables that vanish on restart.
  Persist to a file or a bound service (service_list to see what's
  available).
