# helmdeck

A self-hosted, containerized **Browser-as-a-Service** platform for AI agents — built around **Capability Packs**: typed, schema-validated, one-shot tools that any model (frontier or 7B local) can call by filling in a JSON object.

## Why

Smart models thrive on bash and a README. Weak models stall on open-ended interfaces. Helmdeck closes that gap by hiding browser sessions, desktop actions, credentials, and multi-step workflows behind single typed REST/MCP calls. The defining metric is **≥90% pack success on 7B–30B-class open-weight models.**

## Status

**v0.5.1 shipped** — credential vault, repo packs, security hardening,
code-edit loop, and OpenTelemetry GenAI instrumentation are all live.
The Management UI is mid-rollout (read-only panels for sessions, packs,
MCP, vault; create/edit modals and the killer "model success rates" tab
land in v0.6.0).

- **31 ADRs** in [`docs/adrs/`](docs/adrs/) — every architectural decision with PRD back-references
- **Task breakdown** in [`docs/TASKS.md`](docs/TASKS.md) — ~85 tasks across 8 phases with critical path
- **GitHub milestones** in [`docs/MILESTONES.md`](docs/MILESTONES.md) — drop-in issue checklists with current ship state

## Quick start

```sh
# 1. Clone the repo
git clone https://github.com/tosin2013/helmdeck
cd helmdeck

# 2. Build the Management UI bundle (one-time setup; needs Node 20+)
make web-deps        # npm install
make web-build       # vite build → web/dist/

# 3. Build the control-plane binary with the UI embedded
make build

# 4. Run the control plane with admin credentials
HELMDECK_JWT_SECRET=$(openssl rand -hex 32) \
HELMDECK_VAULT_KEY=$(openssl rand -hex 32) \
HELMDECK_ADMIN_PASSWORD=changeme \
./bin/control-plane

# 5. Open the Management UI
#    URL:      http://localhost:3000
#    Username: admin
#    Password: changeme  (whatever you set above)
```

For the full Compose stack (control plane + Garage object store +
the bundled init service):

```sh
echo "HELMDECK_JWT_SECRET=$(openssl rand -hex 32)"      > deploy/compose/.env
echo "HELMDECK_VAULT_KEY=$(openssl rand -hex 32)"      >> deploy/compose/.env
echo "HELMDECK_ADMIN_PASSWORD=changeme"                >> deploy/compose/.env
docker compose -f deploy/compose/compose.yaml --env-file deploy/compose/.env up -d
```

## Logging in to the Management UI

The login endpoint accepts a static admin password set via the
`HELMDECK_ADMIN_PASSWORD` env var on the control plane process.
Suitable for the dev / single-node Compose tier; OIDC SSO for
production deployments lands in a later phase.

| Setting | Default | Override |
| --- | --- | --- |
| Username | `admin` | `HELMDECK_ADMIN_USERNAME` env var |
| Password | *(none — UI login disabled)* | `HELMDECK_ADMIN_PASSWORD` env var (required) |
| Session length | 12 hours | Hardcoded in `internal/api/auth_login.go` |

**To change the password:** stop the control plane, set
`HELMDECK_ADMIN_PASSWORD` to the new value, and restart. There is
no in-UI "change password" flow today — the password is managed
out-of-band by whichever orchestrator runs the control plane
(Compose, systemd, Kubernetes Secret, etc.).

**If `HELMDECK_ADMIN_PASSWORD` is unset**, the login endpoint
returns `503 login_disabled`. The control plane still runs and the
API still works — operators can mint a JWT directly via the CLI:

```sh
./bin/control-plane -mint-token=alice -mint-token-scopes=admin -mint-token-ttl=12h
```

The minted token can be pasted into any tool that speaks
`Authorization: Bearer <token>`.

**Production note:** the static-password path uses constant-time
comparison so it's safe against timing attacks, but it's still a
shared secret that has to be rotated by hand. For production
deployments with multiple operators, OIDC SSO via your existing
identity provider is the right answer — see the Phase 6 follow-up
roadmap.

## Architecture at a glance

- **Sidecar pattern** — browser runs in its own container, never embedded in the agent (ADR 001)
- **Golang control plane** — single static binary, distroless image, embeds the React UI (ADR 002)
- **Capability Packs** — the primary product surface; user-authorable via Go or WASM (ADRs 003, 012, 024)
- **OpenAI-compatible AI gateway** — Anthropic, Gemini, OpenAI, Ollama, Deepseek with encrypted keys + fallback routing (ADR 005)
- **MCP server registry** — stdio/SSE/WebSocket transports; built-in MCP server auto-derived from the pack catalog (ADR 006)
- **Credential vault** — AES-256-GCM with placeholder-token injection; agents never see secrets (ADR 007)
- **Dual-tier deployment** — Docker Compose for dev/single-node, Helm chart for Kubernetes production (ADRs 009, 010, 011)
- **First-class MCP clients** — Claude Code, Claude Desktop, OpenClaw, Gemini CLI via a single shared `helmdeck-mcp` bridge binary (ADRs 025, 030)
- **Bundled object store** — [Garage](https://garagehq.deuxfleurs.fr/) ships in the Compose stack as the default artifact backend; pluggable to any S3-compatible endpoint (AWS S3, R2, B2, SeaweedFS) for production (ADR 031)

## Built-in Capability Packs

19 packs ship in the box. Each one hides a multi-step workflow
behind a single typed JSON-Schema call so weak open-weight models
can drive it as reliably as frontier models.

| Pack | What it hides |
| :--- | :--- |
| **Browser & web** | |
| `browser.screenshot_url` | Session lifecycle, navigation, render wait, cleanup |
| `web.scrape_spa` | Network-idle wait, schema-driven extraction, validation |
| **Document & vision** | |
| `slides.render` | Marp + Chromium + format flags |
| `doc.ocr` | Tesseract + image preprocessing |
| `desktop.run_app_and_screenshot` | Xvfb + xdotool + scrot + window focus |
| `vision.click_anywhere` | Screenshot → vision model → action loop |
| `vision.extract_visible_text` | Vision model OCR for hard-to-parse pages |
| `vision.fill_form_by_label` | Per-field vision-driven form completion |
| **Code edit loop** | |
| `repo.fetch` / `repo.push` | SSH key selection from vault, `known_hosts`, key shred-on-exit |
| `fs.read` / `fs.write` / `fs.patch` / `fs.list` | Path-safe file ops inside a clone |
| `cmd.run` | Run an arbitrary command in a clone path |
| `git.commit` | Stage + commit attributed to `helmdeck-agent` |
| **Language sidecars** | |
| `python.run` | CPython 3 + pytest + ruff + mypy in a Python sidecar image |
| `node.run` | Node 20 LTS + npm + pnpm + yarn + tsc in a Node sidecar image |
| **HTTP & credentials** | |
| `http.fetch` | Placeholder-token egress: `${vault:NAME}` substitution in URL/headers/body |

See ADRs 014–023 for per-pack contracts and
[`docs/SIDECAR-LANGUAGES.md`](docs/SIDECAR-LANGUAGES.md) for the
runbook on adding new language sidecars (Rust, Go, Ruby, etc.).
The contribution guide in [`CONTRIBUTING.md`](CONTRIBUTING.md)
walks through writing your own pack — the most useful contributions
right now are SaaS API wrappers (Slack, Linear, Stripe, Notion, etc.).

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See
[`NOTICE`](NOTICE) for attribution to bundled and depended-upon
projects, and [`CONTRIBUTING.md`](CONTRIBUTING.md) for the
contribution guide and the SPDX header convention.

By submitting a pull request you agree to license your contribution
under the same terms (Apache 2.0 Section 5 covers the contribution
grant — there's no separate CLA).

## Author

[Tosin Akinosho](mailto:tosin.akinosho@gmail.com) ([@tosin2013](https://github.com/tosin2013))
