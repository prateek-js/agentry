# Sandbox cheat-sheets

Operator-curated, bake-time documentation that lives at
`/etc/sandbox/docs/` inside every sandbox. Read these on your first
turn before reaching for tools — they describe the conventions and
recipes that aren't obvious from the tool descriptions alone.

## Pick a recipe by what the user asked for

| User said something like… | Read |
|---|---|
| "build an agent", "langgraph agent", "tool-using assistant", "chatbot that can …" | `agent.md` |
| "dashboard", "web app", "UI on top of …", "internal tool", anything with a browser frontend | `app.md` + `skills/frontend-design/SKILL.md` |
| "make it look nicer", "redesign", "make it beautiful", anything UI polish | `skills/frontend-design/SKILL.md` + pick a theme in `skills/theme-factory/themes/` |
| "test the app I built", "browser tests", "screenshot the page and check" | `skills/webapp-testing/SKILL.md` |
| "landing page", "microsite", "static HTML page", "marketing site" | `skills/frontend-design/SKILL.md` + `skills/web-artifacts-builder/SKILL.md` |
| "consistent brand voice", "design system across pages" | `skills/brand-guidelines/SKILL.md` |

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

## Design skills (Anthropic-curated)

These live under `skills/<name>/SKILL.md` and contain detailed guidance
on producing distinctive, non-generic UI. Read them whenever you're
about to write CSS, JSX, or shape a page layout:

- `skills/frontend-design/SKILL.md` — bold, intentional aesthetics.
  Avoid AI-slop (Inter font, purple-on-white gradients, predictable
  layouts). Pick a tone and commit.
- `skills/theme-factory/themes/` — 10 ready-to-use themes (arctic-frost,
  botanical-garden, midnight-galaxy, sunset-boulevard, …). Each is a
  full palette + typography + motion spec.
- `skills/brand-guidelines/SKILL.md` — how to keep a brand voice
  consistent across multiple pages or components.
- `skills/webapp-testing/SKILL.md` — testing apps in a real browser
  (Playwright / screenshot validation). Includes scripts.
- `skills/web-artifacts-builder/SKILL.md` — single-file HTML artifacts
  with inline CSS/JS, perfect for landing pages and demos.

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
