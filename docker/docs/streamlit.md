# Recipe: Streamlit data app

For data dashboards, quick exploration UIs, interactive ML demos,
and any "make a UI on top of this notebook" request where Streamlit
is faster than a real frontend.

## Lifecycle (don't skip)

```
project_create  { name: "<short-name>", kind: "streamlit" }
project_start   { name: "<short-name>" }
```

`project_create` writes:
- `/workspace/projects/<name>/.sandbox-project.json` (manifest)
- `/workspace/projects/<name>/app.py` (stub — overwrite with real
  Streamlit code)
- `/workspace/projects/<name>/requirements.txt` (just `streamlit`
  to start — add to it as you import more libs)

`project_start` runs:
```
streamlit run app.py --server.port 8501 --server.address 0.0.0.0 --server.headless true
```

The headless + 0.0.0.0 flags matter: without them Streamlit prompts
for an email on first run and binds loopback-only — the dashboard
proxy can't reach it.

## Before you `command_run "pip install …"`

The runtime image already has the heavy data libs (numpy, pandas,
scikit-learn — check with `pip list`). For anything missing, append
to `requirements.txt` first, then `command_run "pip install -r
requirements.txt"`. Lockstep ordering keeps the project's deps
declarative — restarts pick up whatever's pinned.

## Visual surface

Streamlit is opinionated about layout, but the THEME and tone are
still yours to set. Even before you write a `st.markdown` call:

1. `cat /etc/sandbox/docs/skills/frontend-design/SKILL.md`
2. Pick a theme: `cat /etc/sandbox/docs/skills/theme-factory/themes/<theme>.md`
3. Mirror its palette + typography in Streamlit's theme config (in
   `.streamlit/config.toml`) so the app doesn't look like every
   other Streamlit demo.

Streamlit's defaults look identifiably-Streamlit. Tuning the theme
removes the "this is an LLM-generated dashboard" tell.

## Layout

```
projects/<name>/
  .sandbox-project.json
  app.py                        (entry; the runtime starts this)
  pages/                        (multi-page Streamlit app — optional)
  components/                   (reusable bits per coding-style.md)
  requirements.txt
  .streamlit/config.toml        (theme config, optional)
  data/                         (CSVs, parquet — keep small or use a binding)
```

## When NOT to use streamlit

- The user wants a public-facing site (signup, marketing). Streamlit
  is for internal / data tools. Use `kind: nextjs` or `static-html`.
- The user wants high control over layout/styling. Streamlit's
  layout primitives are limited; a real frontend is faster than
  fighting `st.columns`.
- The user wants real-time push, WebSockets, or a "live" feel —
  Streamlit reruns the whole script on every interaction; this
  doesn't scale to that pattern.

## Anti-patterns

- ❌ `file_write /workspace/projects/foo/.sandbox-project.json` — use
  `project_create`.
- ❌ `command_run "streamlit run app.py &"` — `project_start` owns
  this. Background command bypasses the manager and the dashboard
  can't see the port.
- ❌ "Now run it with `streamlit run app.py`" in chat — the project
  is already running.
- ❌ Skipping `.streamlit/config.toml` — produces the boring default
  theme.
