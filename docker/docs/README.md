# Sandbox cheat-sheets

Operator-curated, bake-time documentation that lives at
`/etc/sandbox/docs/` inside every sandbox. Read these on your first
turn before reaching for tools — they describe the conventions and
recipes that aren't obvious from the tool descriptions alone.

## Two non-negotiable rules

1. **EVERY app runs as a managed project.** Whatever stack you pick,
   it goes through `project_create` → `project_start`. You DO NOT write
   `.sandbox-project.json` by hand and you DO NOT tell the user to run
   the server themselves (no "now run `python3 -m http.server`", no
   "open a terminal and `npm run dev`", no `streamlit run …`
   instructions). The project manager owns the lifecycle, ports,
   restarts, and logs.
2. **EVERY visual surface starts with the design skill.** Before any
   HTML, JSX, or CSS — regardless of what stack you picked — read
   `skills/frontend-design/SKILL.md` and pick a theme from
   `skills/theme-factory/themes/`. Skipping this produces generic
   output the user will ask you to redo. It costs you two tool calls
   and saves you twenty.

These rules apply to every recipe below. They are not stack-specific.

## Pick a recipe by what the user asked for

| User said something like… | Recipe / kind | Read |
|---|---|---|
| "dashboard", "web app", "internal tool", "make it look like X" — anything with multi-page navigation, data fetching, or an API in the same image | `kind: nextjs` | `app.md` + `skills/frontend-design/SKILL.md` |
| "landing page", "marketing site", "microsite", "portfolio", "conference page", "pricing page" — a single page or small set of pages, no backend logic | `kind: static-html` | `static-html.md` + `skills/frontend-design/SKILL.md` |
| "data dashboard", "explore this CSV", "make a quick chart UI", "interactive ML demo" | `kind: streamlit` | `streamlit.md` + `skills/frontend-design/SKILL.md` |
| "API", "endpoint that returns X", "small backend for my client to call" | `kind: fastapi` | `fastapi.md` |
| "worker", "batch job", "scraper", "periodic process" | `kind: python-script` | `python-script.md` |
| Any of the above when the recipe doesn't fit — bespoke stack, custom runtime | `kind: custom` (provide `start_command`) | `projects.md` for manifest schema |
| "make it look nicer", "redesign", "make it beautiful", any UI polish | (any kind) | `skills/frontend-design/SKILL.md` + a theme from `skills/theme-factory/themes/` or a named style in `skills/theme-factory/styles-catalog.md` |
| "looks AI-generated", "looks generic", "looks like every other site" | (any kind) | `skills/frontend-design/references/ai-tells.md` then rebuild the offending parts |
| "build it in <style-name>" (bento, neobrutalist, dark cinema, swiss, sekiro, art deco, longform, …) | (any kind) | `skills/theme-factory/styles-catalog.md` for the exact spec |
| "test the app I built", "browser tests", "screenshot the page" | (any kind) | `skills/webapp-testing/SKILL.md` |
| "consistent brand voice", "design system across pages" | (any kind) | `skills/brand-guidelines/SKILL.md` |
| User EXPLICITLY asked for a single-file HTML ("give me one HTML file I can email") | n/a — emit the artifact, no project | `skills/web-artifacts-builder/SKILL.md` |

All paths produce code that follows `coding-style.md` (80-100 lines
per file, feature-folder layout) and runs as the ONE managed project
on the sandbox under `projects.md` (auto-restart, health, port
discovery — not bare `command_start`).

## Available cheat-sheets

- `coding-style.md` — house rules for the code you produce: 80-100
  lines per file, single responsibility, feature-folder layout.
- `projects.md` — manifest schema reference + tier selection
  (`command_run` vs `command_start` vs `project_start`). Read when
  `project_create` doesn't cover your case and you need `kind:
  custom`.
- `app.md` — RECIPE: scaffold the single Next.js app (App Router +
  TypeScript, API routes and pages in the same image, one process,
  one port, one deploy). Default for app-shaped deliverables.
- `static-html.md` — RECIPE: vanilla HTML/CSS/JS served by Python's
  `http.server`. Default for one-page sites and marketing pages.
- `streamlit.md` — RECIPE: Streamlit data app, requirements pinned,
  managed by the project manager.
- `fastapi.md` — RECIPE: FastAPI + uvicorn, scaffolded with reload.
- `python-script.md` — RECIPE: long-running Python process (worker,
  batch job, scraper).

## Design system

These live under `skills/<name>/SKILL.md` and carry the detailed
guidance on producing distinctive, non-generic UI. Read whenever
you're about to write CSS, JSX, or HTML — regardless of which
project kind you chose:

- `skills/frontend-design/SKILL.md` — bold, intentional aesthetics.
  Avoid AI-slop (Inter font, purple-on-white gradients, predictable
  layouts). Pick a tone and commit.
- `skills/frontend-design/references/ai-tells.md` — concrete patterns
  to refuse: cyan-on-dark, identical card grids, gradient text,
  cream/sand body bg (the 2026 saturated default), aphoristic copy.
- `skills/frontend-design/references/design-rules.md` — hard rules
  across typography, color, contrast, layout, motion, copy.
- `skills/theme-factory/themes/` — 10 ready-to-use tuned palettes
  (arctic-frost, botanical-garden, midnight-galaxy, sunset-boulevard,
  …). Each is a full palette + typography + motion spec.
- `skills/theme-factory/styles-catalog.md` — 50+ named visual languages
  (bento, neobrutalist, dark cinema, swiss, art deco, longform, …)
  with the minimum spec to render each.
- `skills/brand-guidelines/SKILL.md` — how to honor an existing brand
  or establish a new one, then keep it consistent across pages.
- `skills/webapp-testing/SKILL.md` — testing apps in a real browser
  (Playwright / screenshot validation).
- `skills/web-artifacts-builder/SKILL.md` — single-file HTML artifacts
  with inline CSS/JS, perfect for one-off demos.

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
