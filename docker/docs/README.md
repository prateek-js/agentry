# Sandbox cheat-sheets

Operator-curated, bake-time documentation that lives at
`/etc/sandbox/docs/` inside every sandbox. Read these on your first
turn before reaching for tools — they describe the conventions and
recipes that aren't obvious from the tool descriptions alone.

## Pick a recipe by what the user asked for

| User said something like… | Read |
|---|---|
| "build an agent", "langgraph agent", "tool-using assistant", "chatbot that can …" | `agent.md` |
| "dashboard", "web app", "UI on top of …", "internal tool", anything with a browser frontend | `app.md` |

All paths produce code that follows `coding-style.md` (80-100 lines per
file, feature-folder layout) and runs under `projects.md` (managed
projects with `depends_on`, not bare `command_start`).

## Available cheat-sheets

- `coding-style.md` — house rules for ANY app/service/agent you build
  here: 80-100 lines per file, single responsibility, feature-folder
  layout. Applies to every recipe below.
- `projects.md` — how to make a server a managed PROJECT (auto-restart,
  health, port discovery) and how to compose multi-service apps into
  a STACK. Read this before you `command_start` anything you'll touch
  more than once.
- `agent.md` — RECIPE: scaffold a LangGraph agent backed by Anthropic.
  Pinned model (claude-sonnet-4-5), state/nodes/edges/tools split,
  optional HTTP entrypoint, project manifest.
- `app.md` — RECIPE: scaffold a backend+frontend app (FastAPI + Vite
  + React + TypeScript). Two managed projects with `depends_on`
  wiring, feature-folder layout on both sides, dev-proxy + sandbox-proxy
  configuration.

## Credentials and other operator-provided context

If the operator has staged credentials, they live read-only at
`/etc/sandbox/creds/`. The contents are operator-defined — typically
JSON config files for whichever upstream services your code talks to,
sometimes an `env` file that's auto-sourced into every shell, sometimes
a standard `aws/` subdirectory. Discover what's actually there with
`command_run "ls -la /etc/sandbox/creds/"` on your first turn; don't
assume specific filenames.

Read those files IN-CODE (`open("/etc/sandbox/creds/<file>")`). The
HTTP `file_read` / `file_search` / `file_download` endpoints return
**403** under that path so secrets don't leak back through tool
output.

## Reading the docs

Read with `cat`, `head`, or pass the path to `file_read` (the
`/etc/sandbox/docs/*` tree is NOT under the protected `creds/` mount,
so reading is allowed).
