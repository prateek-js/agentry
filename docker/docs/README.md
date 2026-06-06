# Sandbox cheat-sheets

Operator-curated, bake-time documentation that lives at
`/etc/sandbox/docs/` inside every sandbox. Read these on your first
turn before reaching for tools — they describe the conventions and
recipes that aren't obvious from the tool descriptions alone.

## Pick a recipe by what the user asked for

**EVERY visual build is an app.** Landing page, marketing site, portfolio, conference page, microsite, dashboard, internal tool — all route to `app.md` (Next.js). The deploy pipeline ships Next.js apps. There is no "static HTML" shortcut; "no persistence" / "no backend" just means no DB / no Server Actions, not skipping the app template.

| User said something like… | Read |
|---|---|
| "dashboard", "web app", "UI on top of …", "internal tool", "landing page", "marketing site", "microsite", "portfolio", "conference page", "product page", "pricing page", anything visual the user expects to deploy | `app.md` + `skills/frontend-design/SKILL.md` |
| "make it look nicer", "redesign", "make it beautiful", anything UI polish | `skills/frontend-design/SKILL.md` + pick a theme in `skills/theme-factory/themes/` or a named style in `skills/theme-factory/styles-catalog.md` |
| "looks AI-generated", "looks generic", "looks like every other site" | `skills/frontend-design/references/ai-tells.md` then rebuild the offending parts |
| "build it in <style-name>" (bento, neobrutalist, dark cinema, swiss, japanese / sekiro, art deco, longform, …) | `skills/theme-factory/styles-catalog.md` for the exact spec |
| "test the app I built", "browser tests", "screenshot the page and check" | `skills/webapp-testing/SKILL.md` |
| "consistent brand voice", "design system across pages" | `skills/brand-guidelines/SKILL.md` |
| User EXPLICITLY asked for a single-file HTML ("give me one HTML file I can email") | `skills/web-artifacts-builder/SKILL.md` — the ONLY case this skill is the default |
| "build an agent / worker / service / CLI / batch job" | NOT SUPPORTED TODAY — agentry ships only single-image Next.js apps right now. Offer to build the agent/worker/etc AS a Next.js app (logic behind an /api route or a Server Action) so the existing deploy pipeline handles it. |

All paths produce code that follows `coding-style.md` (80-100 lines
per file, feature-folder layout) and runs as the ONE managed project
on the sandbox under `projects.md` (auto-restart, health, port
discovery — not bare `command_start`).

## Available cheat-sheets

- `coding-style.md` — house rules for the app you build here: 80-100
  lines per file, single responsibility, feature-folder layout.
  Applies to every recipe below.
- `projects.md` — how to declare the one project (manifest schema,
  tier selection: `command_run` vs `command_start` vs `project_start`).
  Read this before you `command_start` anything you'll touch more
  than once.
- `app.md` — RECIPE: scaffold the single Next.js app (App Router +
  TypeScript, API routes and pages in the same image, one process,
  one port, one deploy). The default for EVERY visual build.

## Design system

These live under `skills/<name>/SKILL.md` and carry the detailed
guidance on producing distinctive, non-generic UI. Read whenever you're
about to write CSS, JSX, or shape a page layout:

- `skills/frontend-design/SKILL.md` — bold, intentional aesthetics.
  Avoid AI-slop (Inter font, purple-on-white gradients, predictable
  layouts). Pick a tone and commit.
- `skills/frontend-design/references/ai-tells.md` — concrete patterns
  to refuse: cyan-on-dark, identical card grids, gradient text,
  cream/sand body bg (the 2026 saturated default), aphoristic copy.
  Read when reviewing a design that "looks AI-generated".
- `skills/frontend-design/references/design-rules.md` — hard rules
  across typography, color, contrast, layout, motion, copy. Reference
  when in doubt about a specific quantity.
- `skills/theme-factory/themes/` — 10 ready-to-use tuned palettes
  (arctic-frost, botanical-garden, midnight-galaxy, sunset-boulevard,
  …). Each is a full palette + typography + motion spec.
- `skills/theme-factory/styles-catalog.md` — 50+ named visual languages
  (bento, neobrutalist, dark cinema, swiss, art deco, longform, …) with
  the minimum spec to render each. Use when the user names a style
  directly or wants something stronger than "soft modern".
- `skills/brand-guidelines/SKILL.md` — how to honor an existing brand
  or establish a new one, then keep it consistent across pages.
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
