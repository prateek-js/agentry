# Recipe — scaffold a backend + frontend app

Use this when the user asks for "a dashboard", "a web app", "an
internal tool", "a UI on top of …", or any variant involving a browser
UI backed by an API. Defaults: **FastAPI** backend + **Vite + React +
TypeScript** frontend. Don't reach for Next.js, Django, or
create-react-app unless the user explicitly asks — they bring
machinery you don't need for a sandbox demo.

## ⚠️ READ THIS FIRST — scaffold BEFORE you explore

The #1 way this recipe goes wrong is: the model spawns a Jupyter
kernel to "look at the data first", writes all the analytics inline,
maybe drafts a single React component as a one-off, and never produces
two managed projects. At the end there's no `/workspace/projects/backend/`,
no `/workspace/projects/frontend/`, no `.sandbox-project.json` —
just a Jupyter context and some PNG charts. The user can't open it
in a browser, run it on Monday, or deploy it.

DO NOT DO THAT. Rules:

1. The **FIRST `file_write` in this task** is the project skeleton —
   both project manifests. Concretely, before any `code_exec`, before
   any `pip install`, before any `npm install`, you write:
     - `/workspace/projects/backend/.sandbox-project.json`
     - `/workspace/projects/backend/app/__init__.py` (empty)
     - `/workspace/projects/backend/app/main.py` (placeholder)
     - `/workspace/projects/backend/requirements.txt`
     - `/workspace/projects/frontend/.sandbox-project.json` (with `depends_on: ["backend"]`)
     - `/workspace/projects/frontend/package.json`
     - `/workspace/projects/frontend/index.html` (placeholder)
     - `/workspace/projects/frontend/src/main.tsx` (placeholder)
   This commits you to the app being TWO managed projects, with
   ordering encoded in the frontend's `depends_on`.
2. Use `code_exec` for **ad-hoc exploration ONLY** — sample queries, a
   `df.head()`, a quick histogram. ≤30 lines per call. Never put
   business logic, routes, or React components in the kernel.
3. The moment you're writing a real route handler, a service
   function, or a React page: `file_write` it into the matching
   feature folder. The kernel is a scratch buffer, not your codebase.
4. The app isn't "built" until `project_list` reports both projects
   `running` with discovered ports. Empty `project_list` means you
   have to start over. A page rendered in a Claude.ai artifact does
   NOT substitute for a managed project.

### Quoting `pip install` versions

`pip install fastapi>=0.115` runs in bash, which sees `>=0.115` as
output redirection and creates an empty file named `=0.115` in the
current directory. ALWAYS quote pinned versions OR write
`requirements.txt` first via `file_write` and `pip install -r
requirements.txt`. Same applies to `npm install` flags that contain
shell metacharacters.

## Layout — two managed projects

```
/workspace/
├── projects/
│   ├── backend/
│   │   ├── .sandbox-project.json     # type: "service"
│   │   ├── requirements.txt
│   │   ├── README.md
│   │   └── app/
│   │       ├── __init__.py
│   │       ├── main.py               # FastAPI wiring (~40 lines)
│   │       ├── config.py             # env-driven settings (~30 lines)
│   │       ├── deps.py               # shared DI providers (~30 lines)
│   │       ├── data/
│   │       │   ├── __init__.py
│   │       │   └── db.py             # your DB client / API client / store
│   │       ├── <feature>/
│   │       │   ├── __init__.py
│   │       │   ├── routes.py         # HTTP layer (~60-80 lines)
│   │       │   ├── service.py        # business logic (~60-80 lines)
│   │       │   ├── schema.py         # pydantic models (~40 lines)
│   │       │   └── service_test.py
│   │       └── <other-feature>/      # same shape
│   └── frontend/
│       ├── .sandbox-project.json     # type: "app"
│       ├── package.json
│       ├── tsconfig.json
│       ├── vite.config.ts
│       ├── index.html
│       └── src/
│           ├── main.tsx              # entrypoint, mount, router (~30 lines)
│           ├── App.tsx               # top-level layout + routes (~50 lines)
│           ├── api/
│           │   ├── client.ts         # fetch wrapper, baseURL (~40 lines)
│           │   └── <feature>.ts      # per-feature API fns (~50 lines)
│           ├── pages/
│           │   └── <Feature>Page.tsx # one page per route (~80 lines)
│           ├── components/
│           │   └── <Thing>.tsx       # one component per file
│           ├── hooks/
│           │   └── use<Thing>.ts     # one hook per file
│           └── lib/
│               └── format.ts         # tiny shared utilities
```

Ordering between services lives in each project's `depends_on` —
there's no separate stack manifest. `project_start_all` brings the
whole tree up respecting those deps.

Every file stays under ~100 lines. Feature-folder layout, not
layer-folder. See `/etc/sandbox/docs/coding-style.md`.

## Backend — minimum viable

### `requirements.txt`

```
fastapi>=0.115
uvicorn[standard]>=0.30
pydantic>=2
# add domain libs only when you actually use them — your DB driver,
# your HTTP client, boto3, etc.
```

### `app/config.py`

```python
"""Single source for env-driven settings."""
import os
from pydantic import BaseModel

class Settings(BaseModel):
    cors_origins: list[str] = ["*"]
    # Add domain settings here as you add features (DB URL, API base
    # URLs, paths under /etc/sandbox/creds/ that your app cares about).

settings = Settings()
```

### `app/main.py`

```python
"""FastAPI app — wire routers, CORS, health. Keep this thin."""
from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from .config import settings
from .items.routes import router as items_router

app = FastAPI(title="<app-name> API")
app.add_middleware(
    CORSMiddleware,
    allow_origins=settings.cors_origins,
    allow_methods=["*"], allow_headers=["*"],
)

app.include_router(items_router, prefix="/api/items", tags=["items"])

@app.get("/health")
def health():
    return {"ok": True}
```

### `app/data/db.py`

One place to construct your DB / API client and hand it back to
services. Swap the implementation for your real upstream — SQLite,
Postgres, REST API, gRPC, whatever the user actually wants. The
shape stays the same.

```python
"""One place to construct the data backend. Import from here."""
from functools import lru_cache
import sqlite3

@lru_cache(maxsize=1)
def get_conn() -> sqlite3.Connection:
    conn = sqlite3.connect("/workspace/app.db", check_same_thread=False)
    conn.row_factory = sqlite3.Row
    return conn
```

### Per-feature triplet — `items/`

`items/schema.py`:

```python
"""Wire-format types for the items feature."""
from pydantic import BaseModel

class Item(BaseModel):
    id: int
    name: str
    qty: int
```

`items/service.py`:

```python
"""Business logic for the items feature. No HTTP types in here."""
from ..data.db import get_conn
from .schema import Item


def list_items(limit: int = 50) -> list[Item]:
    cur = get_conn().execute(
        "SELECT id, name, qty FROM items ORDER BY id DESC LIMIT ?", (limit,)
    )
    return [Item(**dict(r)) for r in cur.fetchall()]
```

`items/routes.py`:

```python
"""HTTP layer for the items feature. Thin — delegate to service."""
from fastapi import APIRouter, Query
from .schema import Item
from .service import list_items

router = APIRouter()

@router.get("", response_model=list[Item])
def items(limit: int = Query(50, ge=1, le=500)):
    return list_items(limit)
```

### `projects/backend/.sandbox-project.json`

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

Cwd is `/workspace/projects/backend/`, so `app.main:app` resolves to
`/workspace/projects/backend/app/main.py`.

## Frontend — minimum viable (Vite + React + TS)

### `package.json`

```json
{
  "name": "<app-name>-frontend",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "preview": "vite preview"
  },
  "dependencies": {
    "react": "^18.3.1",
    "react-dom": "^18.3.1",
    "react-router-dom": "^6.26.0"
  },
  "devDependencies": {
    "@types/react": "^18.3.3",
    "@types/react-dom": "^18.3.0",
    "@vitejs/plugin-react": "^4.3.1",
    "typescript": "^5.6.0",
    "vite": "^5.4.0"
  }
}
```

### `vite.config.ts`

```ts
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  // `base: "./"` keeps emitted asset paths relative — works whether
  // the operator's tunneling layer fronts you at the root or under a
  // path prefix.
  base: "./",
  server: {
    host: "0.0.0.0",
    port: 5173,
    // Proxy /api → backend so the frontend can use relative URLs and
    // doesn't need CORS in production.
    proxy: { "/api": "http://localhost:8001" },
  },
});
```

### `tsconfig.json`

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "jsx": "react-jsx",
    "strict": true,
    "esModuleInterop": true,
    "skipLibCheck": true,
    "allowImportingTsExtensions": false,
    "noEmit": true
  },
  "include": ["src"]
}
```

### `index.html`

```html
<!doctype html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title><app-name></title>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" src="/src/main.tsx"></script>
  </body>
</html>
```

### `src/main.tsx`

```tsx
import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>,
);
```

### `src/App.tsx`

```tsx
import { Routes, Route, Link } from "react-router-dom";
import OverviewPage from "./pages/OverviewPage";
import TransactionsPage from "./pages/TransactionsPage";

export default function App() {
  return (
    <div className="app">
      <nav>
        <Link to="/">Overview</Link> {" · "}
        <Link to="/transactions">Transactions</Link>
      </nav>
      <main>
        <Routes>
          <Route path="/" element={<OverviewPage />} />
          <Route path="/transactions" element={<TransactionsPage />} />
        </Routes>
      </main>
    </div>
  );
}
```

### `src/api/client.ts`

```ts
// Relative baseURL — Vite proxies /api → http://localhost:8001 in dev.
// In production the operator's tunneling layer fronts both ports under
// the same host, so the same relative path keeps working.
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api${path}`, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return res.json() as Promise<T>;
}
```

### `src/api/items.ts`

```ts
import { api } from "./client";

export type Item = {
  id: number;
  name: string;
  qty: number;
};

export function fetchItems(limit = 50) {
  return api<Item[]>(`/items?limit=${limit}`);
}
```

### `src/pages/ItemsPage.tsx`

```tsx
import { useEffect, useState } from "react";
import { Item, fetchItems } from "../api/items";

export default function ItemsPage() {
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    fetchItems().then(setItems).catch((e) => setError(String(e)));
  }, []);

  if (error) return <pre style={{ color: "crimson" }}>{error}</pre>;
  if (!items) return <p>Loading…</p>;
  return (
    <section>
      <h1>Items</h1>
      <ul>
        {items.map((it) => (
          <li key={it.id}>{it.name} — qty {it.qty}</li>
        ))}
      </ul>
    </section>
  );
}
```

### `projects/frontend/.sandbox-project.json`

```json
{
  "name": "frontend",
  "type": "app",
  "start_command": ["npm", "run", "dev", "--", "--host", "0.0.0.0", "--port", "5173"],
  "auto_restart": true,
  "depends_on": ["backend"]
}
```

## Recipe — end-to-end

Do these in order. **Don't skip step 0.**

0. **SCAFFOLD FIRST (before any exploration, install, or feature
   code).** `file_write` both project manifests and the placeholder
   files listed in the rules block above. After this step:

   ```bash
   command_run "ls -R /workspace/projects"
   ```

   should show both project trees. The app exists as a project shape.
   Now you can explore safely without losing track.

1. **Confirm the data shape.** If the app reads from an upstream (DB,
   API, file dump, …), open a Jupyter context and probe the schema
   first — a few sample rows, the column types, the rough cardinality.
   Use `code_exec` for this — but each call <30 lines, throwaway
   only. The moment you want to keep a query, `file_write` it into
   `backend/app/<feature>/service.py`.

2. **Sketch the feature list.** 3-5 features max for v1. Each becomes a
   feature folder in both backend (`app/<feature>/`) and frontend
   (`pages/<Feature>Page.tsx` + `api/<feature>.ts`).

3. **Backend, in order**: `requirements.txt`, `app/config.py`,
   `app/data/db.py` (or the equivalent client for your upstream), then
   per-feature `routes.py`+`service.py`+`schema.py`, then `app/main.py`
   last (it imports everything). Update the placeholder `main.py`
   from step 0 in place.

4. `command_run "cd /workspace/projects/backend && pip install -r requirements.txt"`.

5. **Frontend, in order**: replace the placeholder `package.json` /
   `index.html` / `src/main.tsx` from step 0, then add
   `tsconfig.json`, `vite.config.ts`, `src/App.tsx`,
   `src/api/client.ts`, then per-feature `api/<f>.ts` +
   `pages/<F>Page.tsx`.

6. `command_run "cd /workspace/projects/frontend && npm install"`
   (takes ~30-60 s on a cold cache).

7. **Finalise manifests**: revisit `backend/.sandbox-project.json` and
   `frontend/.sandbox-project.json` from step 0 and update their
   `start_command` / `health_check` now that the entrypoints exist.
   Make sure `frontend.depends_on = ["backend"]` so ordering is
   respected.

8. `project_start_all` — the manager brings up backend first
   (depends_on chain), then frontend, both with auto-restart.

9. Verify:
   - `project_list` → both projects `running`, ports `[8001]` and
     `[5173]` discovered, both `healthy`.
   - `curl http://localhost:8001/health` (inside the sandbox via
     command_run) → `{"ok": true}`.

10. Tell the user how to open the frontend. The recipe is ALWAYS
    these two lines — do not construct any URL yourself, do not
    paste the sandbox_url, do not build paths off broker.invalid:

       Run this in another terminal:
           xdp forward <sandbox-id>:5173
       then open http://localhost:5173/ in your browser.

    Substitute the actual sandbox id. Don't keep the chat going to
    "test" — that's the user's job. Stop.

### The exit check

Before you tell the user "the app is up," run:

```bash
command_run "ls -R /workspace/projects/backend /workspace/projects/frontend"
command_run "wc -l /workspace/projects/backend/app/**/*.py /workspace/projects/frontend/src/**/*.{ts,tsx}"
# and:
project_list(sandbox_url=...)
```

You should see: both project trees on disk, every file under ~100
lines, and both projects running with discovered ports. If any of
those three checks fail, you haven't finished — keep going. A
Claude.ai artifact is NOT a substitute for a managed project; if the
only thing you produced is a React page rendered in the chat, you
built nothing.

## Frontend gotchas under operator tunneling

The operator's tunneling layer may front the frontend on a subpath or
a fresh subdomain — write code that survives either. Defenses:

- `vite.config.ts` has `base: "./"` so emitted JS/CSS paths are
  relative.
- Use `react-router-dom`'s `BrowserRouter`; if the tunnel uses a
  subpath and route-not-found shows up, swap to `HashRouter` — paths
  like `/#/transactions` survive any prefix.
- API calls should be relative (`fetch("/api/...")`) so the same code
  runs in dev (Vite proxy) and behind whatever the operator's tunnel
  routes through.

## Common pitfalls

- **Don't put SQL strings in `routes.py`** — they belong in
  `service.py`. The HTTP layer should be unit-testable without a
  database mock.
- **Don't write a 400-line `App.tsx`.** Pages go in `pages/`,
  components in `components/`. `App.tsx` is just the router.
- **Don't `pip install` from inside the running uvicorn process.** Stop
  the project first (`project_stop`), install, restart.
- **Don't open a fresh upstream connection per request.** `lru_cache`
  the client; let the driver pool or your HTTP session handle reuse.
  Fresh connections can cost hundreds of milliseconds each.
- **Don't reach for Next.js.** A SPA behind an operator tunnel is
  simpler; Next's SSR + routing assumptions fight subpath prefixes if
  the tunnel uses one.
- **Don't drive multiple `project_start` calls by hand.** Use
  `project_start_all` for the whole tree — one tool call, dependency
  order respected via each project's `depends_on`.
