# Recipe: FastAPI

For small APIs, internal endpoints, JSON services — anything where
the user wants "a backend" but not a full app.

## Lifecycle (don't skip)

```
project_create  { name: "<short-name>", kind: "fastapi" }
project_start   { name: "<short-name>" }
```

`project_create` writes:
- `/workspace/projects/<name>/.sandbox-project.json`
- `/workspace/projects/<name>/app.py` (stub — overwrite)
- `/workspace/projects/<name>/requirements.txt` (`fastapi`,
  `uvicorn[standard]`)

`project_start` runs:
```
uvicorn app:app --host 0.0.0.0 --port 8000 --reload
```

The `--reload` flag means edits to `.py` files are picked up without
a restart — good for iteration, but the project manager's
auto-restart still applies if the process actually crashes.

## Visual surface

FastAPI is API-only by default — there's no UI to design. But:

- If you add `/docs` (FastAPI gives this for free), the Swagger UI
  uses default theming. Fine for internal use.
- If the user wants a frontend AND an API in the same deliverable,
  use `kind: nextjs` instead. Next.js handles both. Don't run a
  FastAPI app and a separate frontend project in the same sandbox —
  one project per sandbox is the rule.

If you DO produce HTML responses (templated or otherwise), the
frontend-design skill applies — read it before writing CSS.

## Layout

```
projects/<name>/
  .sandbox-project.json
  app.py                        (FastAPI entry)
  routes/                       (group endpoints by feature per coding-style.md)
  models/                       (Pydantic schemas)
  services/                     (DB access, external API calls)
  requirements.txt
```

## When NOT to use fastapi

- The user wants a UI + an API in one deliverable. `kind: nextjs`
  gives you both in one process.
- The user wants a "data app" / "let me explore this CSV". Use
  `kind: streamlit` instead.
- The user wants a worker / batch job (no incoming requests). Use
  `kind: python-script`.

## Anti-patterns

- ❌ `file_write .sandbox-project.json` — use `project_create`.
- ❌ `command_run "uvicorn app:app … &"` — `project_start` owns it.
- ❌ "Now hit http://localhost:8000/docs" in chat — the user reaches
  it via the dashboard URL.
- ❌ Two projects in one sandbox (FastAPI backend + Next.js
  frontend) — one project per sandbox. Use Next.js's API routes if
  you need both.
