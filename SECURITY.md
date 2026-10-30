# Security Policy

English | [简体中文](SECURITY.zh-CN.md)

GPUWorks manages GPU hosts, runs inference containers on them, and stores credentials for
cloud providers and model sources. A vulnerability here can reach beyond a single dashboard:
it can become remote code execution on every node in a cluster, or disclosure of the keys used
to rent and control machines.

We take reports seriously and prefer to hear about a problem early, even if it is not fully
worked out.

## Reporting a vulnerability

**Do not open a public issue, and do not post details in a pull request, a discussion, or
any chat channel.**

Use either of these private channels:

1. **GitHub private vulnerability reporting** — the *Report a vulnerability* button under the
   repository's *Security* tab (preferred: it keeps the thread, the fix and the advisory
   linked).
2. **Email** — **security@gpuworks.ai**.

Encrypting the report with our PGP key is welcome but not required for a first contact; if you
want to send proof-of-concept code, say so and we will arrange a channel.

### What to include

The faster we can reproduce, the faster we can fix. Please provide:

- **Version** — the output of `curl -s http://<server>:8800/healthz` (`dev-<commit>`), or the
  commit you built from.
- **Deployment shape** — single node or multi-node, how the agent connects (agent dials out, or
  `server_dial`), container runtime, and whether the instance is reachable from the internet.
- **Impact** — what an attacker gains: credential disclosure, node compromise, cross-tenant
  access, denial of service, and so on.
- **Reproduction** — the smallest sequence that demonstrates it. A script or a set of curl
  commands is ideal.
- **Evidence** — relevant log excerpts with secrets removed.

If you are unsure whether something is in scope, report it and let us decide. A report that
turns out to be by design costs us a few minutes; a missed real issue costs users their
clusters.

## Our commitment

This is a small team, so we commit to a process rather than to a contractual SLA:

| Stage | Target |
|---|---|
| Acknowledge the report | within 5 working days |
| Initial assessment (in scope / severity / whether a fix is feasible) | within 10 working days |
| Fix or mitigation for a confirmed high-severity issue | as fast as we can, with progress updates at least every 2 weeks |
| Public disclosure | coordinated with you, after a fix or mitigation is available |

If a report is out of scope or is not a vulnerability, we will say so and explain why, rather
than closing it silently.

We will credit reporters in the advisory unless you ask us not to.

## Supported versions

GPUWorks releases are rolling development builds named `dev-<commit>`; there is no long-term
support branch yet. Security fixes land on the main branch and are included in the next build.

If you are running an older build, the first thing we will ask is whether the issue reproduces
on the latest one. Please check that before reporting, when it is practical.

## Scope

In scope — examples, not an exhaustive list:

- Authentication or authorisation bypass in the API, the gateway, or the web UI
- Credential or token disclosure through API responses, logs, error messages, or exported
  diagnostics
- Server-side request forgery that reaches internal services or cloud metadata endpoints
- Container escape, or breaking the isolation between deployments sharing a node
- Arbitrary file read or write on a node or on the server
- Remote code execution through model files, templates, image references, or agent commands
- Privilege escalation from a low-privilege account to admin

Out of scope:

- Attacks that require existing root or administrative access to the host
- Denial of service caused by a single authenticated user exhausting their own quota
- Reports produced solely by an automated scanner without a demonstrated impact
- Missing security headers, or TLS configuration of a deployment you control
- Default credentials of a test environment that the documentation already tells you to change
- Social engineering, or physical access to hardware

## Security-relevant design

These are the parts of the system where a finding would be most valuable, and where the code
already tries to defend itself:

- **Outbound HTTP policy** (`internal/server/outbound`) — user-configurable sources are fetched
  through a transport that validates every DNS answer, rejects private, loopback, link-local
  and cloud-metadata ranges, pins the resolved address to the socket, re-checks redirects, and
  deliberately does not echo resolver or network errors back to the caller.
- **Provider credentials** — cloud account credentials are stored AES-GCM encrypted, and the
  API never returns secrets; only non-secret fields are echoed back for editing.
- **`server_dial`** — the reverse connection used for nodes that cannot accept inbound traffic
  is authenticated with HMAC.
- **Log and diagnostic redaction** — business logs are expected to exclude tokens, and request
  bodies are redacted recursively before being written.

If you find a gap between the description above and the behaviour you observe, that is exactly
the kind of report we want.

## After a fix

For confirmed vulnerabilities we will publish an advisory describing the affected versions,
the impact, and the fix, once a fixed build is available and users have had a reasonable window
to upgrade. We are happy to coordinate timing with you.
