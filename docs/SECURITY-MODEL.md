# Security model

agentry is built so that **the things that matter to you — your code, your
data, your credentials — stay on hardware you control, and every connection
between components is authenticated.** This page describes the posture at a
high level. It is intentionally about *what* we guarantee, not a blueprint
of *how*.

## Principles

**Your hardware is the boundary.** Sandboxes run on your machine or your
server. Source, data, running apps, and credentials live there. The
platform is the front door that wires things together — not the host of your
work.

**Zero trust between components.** No component is trusted because of where
it sits on the network. Each hop — your AI client, the routing layer, the
host agent, the sandbox runtime — proves its identity before the next one
will talk to it. Being able to reach a port is never enough; you also have
to be who you claim to be.

**The routing layer is a blind pivot.** The component that connects a remote
client to compute behind a firewall routes by verified identity and holds no
durable state about your traffic. It is not a place your data comes to rest.

**Secrets bypass the platform entirely.** Service bindings and environment
variables are wired directly into the runtime on your infrastructure and are
never transmitted to or stored by any agentry-operated service. See
[Services & environment model](SERVICE-AND-ENV-MODEL.md).

**Least privilege, by default.** Components listen only where they need to,
the host agent is the only thing permitted to drive a sandbox's runtime, and
sandboxes are isolated from each other and the host by the container backend.
Defaults are chosen to be safe rather than convenient.

**Defense in depth.** No single control is load-bearing on its own —
network scoping, authenticated identity, and runtime gating each stand on
their own, so one misconfiguration doesn't open the door.

## Scope & honesty

agentry today is designed for a single operator running it on their own,
mostly-trusted infrastructure. That's the model the defaults target.
Stronger isolation for running fully untrusted code (microVM / sandboxed
kernels) and turnkey multi-tenant hardening are on the roadmap, not
shipped — so we don't claim them. We'd rather state the boundary plainly
than over-promise.

## For contributors

The component-level mechanics (transport, identity, runtime gating, sandbox
isolation) are described in [ARCHITECTURE.md](ARCHITECTURE.md#security-model)
for people working on the code.

## Reporting a vulnerability

Please report security issues privately — see [SECURITY.md](../SECURITY.md).
Do not open a public issue.
