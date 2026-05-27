# Coding style — modular, small files

These are the operator's house rules for any app, service, agent,
notebook, or script you build inside the sandbox. Follow them by
default for every `file_write`; only deviate if the user explicitly
asks for a single-file scratchpad.

## The size rule

**80–100 lines per file, max.** A file that grows past 100 lines is
your signal that it's doing more than one thing — split it.

- 80 lines is the target.
- 100 lines is the soft cap (occasional overflow ok).
- Past 120 lines: **stop and split** before adding more.

A "line" means a non-blank, non-comment line of code. Long imports,
docstrings, and configuration tables don't count toward the budget —
they aren't where complexity hides.

## What "modular" looks like

- **One responsibility per file.** Name the file after the thing it
  exports. A file called `auth.py` exports auth; if it also has the
  HTTP routes that use it, those routes belong in `auth_routes.py`.
- **One entrypoint per program.** `main.py` / `app.py` / `index.ts`
  just wires modules together; it doesn't define them.
- **Co-locate by feature, not by kind.** Prefer `users/handlers.py`,
  `users/service.py`, `users/schema.py` over `handlers/users.py`,
  `services/users.py`, `schemas/users.py`. Features change together;
  layers don't.
- **Tests live next to the code they cover.** `foo.py` →
  `foo_test.py` in the same directory.

## Concrete file budgets

- **Python module**: imports + 2-4 top-level functions/classes per
  file. If a class has more than ~5 methods or any method is more than
  ~30 lines, split.
- **React/Vue component**: one component per file, named after the
  component. Hooks → `useThing.ts`, helpers → `thing.utils.ts`,
  types → `thing.types.ts`, styles → `Thing.module.css`.
- **Go package**: one type per file when the file has methods on it.
  `handler.go` defines the handler; `handler_session.go` adds
  session-specific methods on the same type if you need more space.
- **SQL**: one logical query (or DDL group) per file. Migrations are
  numbered + named: `001_create_users.sql`.

## Typical project tree (Python service)

```
my-service/
├── pyproject.toml
├── README.md
├── app/
│   ├── __init__.py          # 1-2 lines, re-export the FastAPI app
│   ├── main.py              # ~40 lines: wire routes, run uvicorn
│   ├── config.py            # ~30 lines: env-driven settings
│   ├── deps.py              # ~30 lines: shared DI providers
│   ├── users/
│   │   ├── __init__.py
│   │   ├── routes.py        # ~80 lines: HTTP layer
│   │   ├── service.py       # ~80 lines: business logic
│   │   ├── schema.py        # ~40 lines: pydantic models
│   │   └── service_test.py
│   └── billing/             # same shape as users/
│       └── …
└── tests/                   # cross-feature integration tests only
```

If a single file in `users/` would cross 100 lines, split by the next
natural seam (e.g. `service.py` → `service_create.py` +
`service_query.py`), not by adding line-shaving comments.

## pip install — always quote version pins

```bash
# WRONG — bash sees `>=2.0` as redirection and creates an empty
# file named `=2.0` in cwd. The package is NOT installed.
pip install langgraph>=0.2.50 langchain-anthropic>=0.3 pydantic>=2

# RIGHT — quote each pinned spec.
pip install "langgraph>=0.2.50" "langchain-anthropic>=0.3" "pydantic>=2"

# BETTER — write requirements.txt with file_write, then:
pip install -r requirements.txt
```

If you see `=0.x` / `=2` / `=1.0.0`-named files in `/workspace/` after
an install, that's the bash-redirection bug. Delete them and re-run
with quotes.

## How to put bytes on disk — always `file_write`

When building a project, every new file goes through `file_write`. One
call per file. Don't reach for `command_run` to write files — heredocs
(`cat > app.py <<EOF`), `tee`, `printf … > app.py`, `echo … > app.py`,
or `python3 -c "open(...).write(...)"` all push the content through a
bash PTY (~100–300 ms each). `file_write` is a single HTTP POST that
calls `os.WriteFile` directly (~5 ms).

You also don't need `mkdir -p` first — `file_write "/a/b/c/foo.py"`
creates `/a/b/c/` automatically.

`command_run` is for `pip install`, `pytest`, `git`, `curl`, build
commands — things that *do* something. Putting bytes on disk is not a
"thing it does."

## Anti-patterns to refuse

- A single `app.py` that holds routes, DB models, business logic, and
  HTML templates. Split it on the first commit.
- A `utils.py` / `helpers.py` that grows without a theme. If it's past
  60 lines or has more than ~4 unrelated functions, give each function
  family its own file (`time_utils.py`, `url_utils.py`, …) or fold it
  into the feature that uses it.
- "I'll refactor later" — refactor BEFORE you write more code into the
  file, not after.
- Bundling React JSX, fetch logic, and styles into one 400-line
  component "to keep things together." It's not together; it's tangled.

## When to deviate

Keep the rule unless the user explicitly says one of:

- "make this a single-file script"
- "give me a one-pager"
- "I just want the gist in one file"

A throwaway scratchpad or a single-file CLI demo is a legitimate
exception; a real app is not.

## Self-check before you stop writing

After your last `file_write`, run:

```bash
find . -name '*.py' -o -name '*.ts' -o -name '*.tsx' -o -name '*.go' \
  | xargs wc -l | sort -rn | head -20
```

Any file over 120 lines is your TODO before you tell the user you're
done.
