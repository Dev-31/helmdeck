# helmdeck

A self-hosted, containerized **Browser-as-a-Service** platform for AI agents — built around **Capability Packs**: typed, schema-validated, one-shot tools that any model (frontier or 7B local) can call by filling in a JSON object.

## Why

Smart models thrive on bash and a README. Weak models stall on open-ended interfaces. Helmdeck closes that gap by hiding browser sessions, desktop actions, credentials, and multi-step workflows behind single typed REST/MCP calls. The defining metric is **≥90% pack success on 7B–30B-class open-weight models.**

## Status

Pre-implementation. Architecture is locked; phase 1 starts next.

- **30 ADRs** in [`docs/adrs/`](docs/adrs/) — every architectural decision with PRD back-references
- **Task breakdown** in [`docs/TASKS.md`](docs/TASKS.md) — ~75 tasks across 8 phases with critical path
- **GitHub milestones** in [`docs/MILESTONES.md`](docs/MILESTONES.md) — drop-in issue checklists
- **Release plan** in [`docs/RELEASES.md`](docs/RELEASES.md) — what ships when, with hard exit gates

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

| Pack | What it hides |
| :--- | :--- |
| `browser.screenshot_url` | Session lifecycle, navigation, render wait, cleanup |
| `web.scrape_spa` | Network-idle wait, schema-driven extraction, validation |
| `web.login_and_fetch` | Vault credential injection, session, cookies |
| `web.fill_form` | Form detection, vault injection, confirmation |
| `slides.render` | Marp + Chromium + format flags |
| `slides.video` | Marp + Xvfb + ffmpeg + TTS + muxing |
| `desktop.run_app_and_screenshot` | Xvfb + xdotool + scrot + window focus |
| `doc.ocr` | Tesseract + image preprocessing |
| `repo.fetch` / `repo.push` | SSH key selection, known_hosts, HTTPS↔SSH normalization, retries |

See ADRs 014–023 for per-pack contracts.

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
