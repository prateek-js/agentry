# agentry — MCP server listing

Copy-paste source for submitting agentry to MCP directories and harness
marketplaces (awesome-mcp-servers, the MCP registry, Cursor/Claude
directories). Keep this in sync with the live product.

---

**Name:** agentry

**One-liner:** Self-hosted infrastructure so any AI coding agent can build, run, and deploy apps on a server you own — your code and data never leave your hardware.

**Homepage:** https://agentry.run
**Docs:** https://docs.agentry.run
**Categories:** sandbox · code-execution · deployment · self-hosted · devops · infrastructure

## Description

agentry connects an AI coding agent (Claude Code, Cursor, Roo, or any MCP
client) to a machine you control — your laptop, a $5 VPS, bare metal. Over
MCP the agent gets a real sandbox on that machine: filesystem, shell,
package managers, plus any databases or API keys you've bound. It builds
against the real thing instead of stubs, then one click ships the result as
a container served at a public HTTPS URL.

What makes it different:

- **Your hardware, your data.** Code, databases, and running apps stay on
  the machine you connect — agentry is the front door, not the host.
- **Bring your own model, no markup.** The agent uses your API key; agentry
  never proxies or resells tokens.
- **Zero inbound ports.** The server dials out over an mTLS tunnel — nothing
  to firewall, port-forward, or expose.
- **One box, many apps.** A single small server runs all the internal tools
  your team builds; the bill doesn't grow per app.

## Install

```bash
curl -fsSL https://agentry.run/install.sh | sh
agentry login
# add a server from the dashboard's "Add this machine" panel, then:
agentry server use <name>
```

## MCP client config

```json
{
  "mcpServers": {
    "agentry": {
      "command": "agentry",
      "args": ["mcp"]
    }
  }
}
```

Pin one session to a specific server (e.g. one editor on `laptop`, another
on `production`):

```json
"args": ["mcp", "--server", "<name>"]
```

## Representative tools

`sandbox_create` · `file_write` / `file_read` / `file_list` / `file_grep` /
`file_replace` · `command_run` / `command_start` / `command_logs` ·
`project_create` / `project_start` / `project_stop` / `project_logs` ·
`port_wait` · `service_bind` · `app_probe` · `code_exec` ·
`deployment_status`

Full reference: https://docs.agentry.run/reference/mcp-tools
