# Service catalog

Each YAML in this directory defines one service users can bind to a
sandbox. Binding stamps environment variables and credential files
into the sandbox so the user's code reaches the service without any
config-hunting.

## Manifest shape

```yaml
name: <slug>              # identifier (lowercase, dash-separated)
display_name: <string>    # human-friendly name shown in UI
category: <enum>          # database | cache | storage | search | email |
                          # payments | ai | analytics | messaging | other
description: |
  One- or two-line description of what the service is and when to use it.

fields:                   # what the user must supply to bind this service
  - name: <slug>          # used as a placeholder key in `inject`
    label: <string>       # shown in UI prompt
    placeholder: <string> # optional, shown as gray text
    default: <string>     # optional, pre-filled value
    secret: <bool>        # optional, mask input in UI + redact in logs
    required: <bool>      # optional, default true
    prod_required: <bool> # optional. If true, `agentry promote` forces a
                          # new value at deploy time (never let the dev
                          # value leak into prod).
    pattern: <regex>      # optional, client-side validation

inject:                   # what gets stamped into the sandbox
  env:                    # key=value pairs added to the sandbox env. All
    KEY1: "{field-name}"  # bound services merge into one env namespace.
    KEY2: "literal value"
  creds:                  # files written under /etc/sandbox/creds/
    /etc/sandbox/creds/<name>/<file>: |
      file contents with {field-name} interpolation

get_started: |            # optional. Markdown shown in UI to help users
                          # find a connection URL fast.
```

## Layered lookup

Provisioner loads manifests from (later wins):

1. **Built-in** — bundled with the provisioner binary (this directory)
2. **Org-defined** — fetched from the control plane (per-org overrides)
3. **Cluster-local** — `/etc/agentry/services/*.yaml` on the cluster
   host. Operator drops a yaml, restart provisioner, new service
   available.

Adding a new service = drop a yaml. No code changes.

## Convention

- Env var names follow what the upstream library expects by default
  (e.g. `DATABASE_URL` for postgres, `REDIS_URL` for redis, the
  standard `AWS_*` for AWS). Code the LLM writes against these names
  works in dev AND in prod with no diff.
- Cred files at `/etc/sandbox/creds/<service-name>/...` so it's
  predictable from the file tree alone.
- Use `prod_required: true` on anything you absolutely don't want a
  developer's test key leaking into a production deployment (Stripe
  secret keys, OpenAI keys, prod DB URLs).
