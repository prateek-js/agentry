# Recipe: static HTML

For one-page sites, landing pages, marketing pages, portfolios,
conference pages, microsites — anything where you'd otherwise be
tempted to tell the user "just run `python3 -m http.server`".

**Prerequisite: `docs_read("CONTRACT")` — bind `$PORT` (rule 1), bind
services before coding against them (rule 3), report platform
problems instead of patching around them (rule 5).**

The scaffolded `start_command` binds `"${PORT:-8000}"` via `sh -c` —
leave the variable in place (CONTRACT rule 1).

## Lifecycle (don't skip)

```
project_create  { name: "<short-name>", kind: "static-html" }
project_start   { name: "<short-name>" }
```

`project_create` writes:
- `/workspace/projects/<name>/.sandbox-project.json` (manifest, do
  NOT edit it directly — use `kind: custom` if the start command
  needs to change)
- `/workspace/projects/<name>/index.html` (stub — overwrite)

`project_start` runs `python3 -m http.server 8000` from the project
dir. Port 8000 is bound; the dashboard discovers it; the LLM does
NOT issue `command_run` to start the server.

## Visual surface — read FIRST

Before you write a single line of HTML, CSS, or JS:

1. `cat /etc/sandbox/docs/skills/frontend-design/SKILL.md`
2. Pick a theme: `ls /etc/sandbox/docs/skills/theme-factory/themes/`
   then `cat /etc/sandbox/docs/skills/theme-factory/themes/<theme>.md`
3. If the user named a style ("bento", "sekiro", "neobrutalist"…),
   look it up in `skills/theme-factory/styles-catalog.md`.

Skipping this step produces generic cyan-on-dark slop. The user
will ask you to redo it. The two-tool-call cost up front is far
cheaper than a rebuild.

## Layout

```
projects/<name>/
  .sandbox-project.json         (managed by project_create — don't edit)
  index.html                    (your entry page)
  assets/                       (CSS, JS, images, fonts; reference relatively)
  about.html, work.html, …      (additional pages — Python's http.server serves them)
```

Keep CSS/JS in `assets/style.css` + `assets/main.js` per
coding-style.md — inlining everything in a 1000-line `<style>` block
becomes unreadable fast. The exception is small artifacts the user
asked for as a single file; for those use the
`web-artifacts-builder` skill instead and skip the project.

## When NOT to use static-html

- The user wants a backend (auth, DB writes, server-side rendering,
  Server Actions, file uploads). Use `kind: nextjs` instead.
- The user wants real-time / WebSockets / SSE. Use `kind: nextjs`
  or `kind: fastapi` + a small static frontend.
- The user asked for "an app" with multi-page navigation, shared
  state, or anything that benefits from a router. Default
  `kind: nextjs`.

## Anti-patterns

- ❌ `file_write /workspace/projects/foo/.sandbox-project.json` — use
  `project_create` instead.
- ❌ `command_run "python3 -m http.server 8000 &"` — `project_start`
  owns this.
- ❌ "Now open http://localhost:8000 in your browser" — the user
  reaches the running sandbox via the dashboard URL, not localhost.
- ❌ Writing CSS before reading `skills/frontend-design/SKILL.md`.
