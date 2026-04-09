# Helmdeck Client Integrations

Setup guides for connecting MCP-capable clients to a Helmdeck control plane via the `helmdeck-mcp` stdio bridge.

Every client speaks the same wire protocol (per ADR 025); the only per-client variation is the config-file shape and the install ergonomics. Each guide below walks through:

1. **Prerequisites** — bridge binary, control-plane URL, JWT
2. **Install the bridge** — Homebrew / Scoop / npm / OCI / `go install`
3. **Configure the client** — exact config snippet (from `/api/v1/connect/{client}`)
4. **Phase 5.5 code-edit-loop walkthrough** — `repo.fetch` → `fs.list` → `fs.read` → `fs.patch` → `cmd.run` → `git.commit` → `repo.push` against a real private repo
5. **Troubleshooting**

Each guide carries a **status banner** at the top using the legend below.

## Status legend

| Badge | Meaning |
| :--- | :--- |
| ✅ **Tested & integrated** | A maintainer has walked the full Phase 5.5 code-edit loop end-to-end against a real private GitHub repo with this client. Date + Helmdeck version recorded in the banner. |
| 🟡 **Documented, not yet verified** | Setup instructions are written and believed correct, but the Phase 5.5 loop has not been walked end-to-end with this client yet. |
| ⚪ **Planned** | Stub page exists; setup not yet documented. |

## Client matrix

| Client | Guide | Status |
| :--- | :--- | :--- |
| Claude Code | [claude-code.md](claude-code.md) | 🟡 Documented, not yet verified |
| Claude Desktop | [claude-desktop.md](claude-desktop.md) | 🟡 Documented, not yet verified |
| OpenClaw | [openclaw.md](openclaw.md) | 🟡 Documented, not yet verified |
| Gemini CLI | [gemini-cli.md](gemini-cli.md) | 🟡 Documented, not yet verified |
| NemoClaw | [nemoclaw.md](nemoclaw.md) | 🟡 Documented (reuses OpenClaw schema inside the sandbox) |
| Hermes Agent | [hermes-agent.md](hermes-agent.md) | 🟡 Documented, not yet verified |

> When a client is promoted to ✅, update both its page banner **and** the row in this matrix. Keep them in sync.

## Manual validation helper

`scripts/validate-clients.sh` (T564) boots the compose stack and prints the connect snippets + a copy-pasteable JSON-RPC scenario for the code-edit loop. Use it as scaffolding while walking a client through the loop by hand — there is no automated pass/fail.
