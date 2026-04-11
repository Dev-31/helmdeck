# OpenClaw

> **Status:** ✅ Verified end-to-end on 2026-04-10 (helmdeck v0.6.0)
> Validated via `scripts/validate-openclaw.sh` — 9 packs tested through OpenClaw → SSE MCP → helmdeck round trip with `openrouter/auto` as the LLM. Packs validated: `http.fetch`, `browser.screenshot_url`, `web.scrape_spa`, `slides.render`, `browser.interact`, `github.list_prs`, `github.list_issues`, `github.search`, `repo.fetch` + `fs.list` chain. Additionally validated via direct REST: full code-edit loop (`repo.fetch` → `fs.write` → `fs.patch` → `fs.read` → `cmd.run` → `git.commit` → `repo.push`) + all GitHub write packs (`create_issue`, `post_comment`, `create_release`) + `python.run` + `node.run`.

## Topology

OpenClaw is **Topology A** — both OpenClaw and helmdeck run as docker compose stacks on the same host, joined onto a shared bridge network so OpenClaw resolves `helmdeck-control-plane` by service-name DNS.

```
┌──── helmdeck_default network ─────────┐
│  helmdeck-control-plane:3000          │
│  ┌────────────────────────────┐       │
│  │ /api/v1/mcp/sse  (MCP)     │       │
│  │ /v1/chat/completions (LLM) │       │
│  └────────────────────────────┘       │
│            ▲                          │
│            │ HTTP, JWT-protected      │
│  openclaw-gateway:18789               │
└───────────────────────────────────────┘
```

## Prerequisites

- Docker + docker compose v2
- Helmdeck cloned at `/root/helmdeck` (or wherever)
- ≥ 4 GB RAM, ≥ 2 CPUs (the install script preflight enforces this)

## 1. Install helmdeck

```bash
git clone https://github.com/tosin2013/helmdeck.git
cd helmdeck
./scripts/install.sh
```

The script generates a `.env.local` with strong random secrets, builds every binary + the React UI, brings the compose stack up, polls `/healthz`, and prints the admin password. Save it.

## 2. Install OpenClaw

```bash
git clone https://github.com/openclaw/openclaw.git
cd openclaw
OPENCLAW_GATEWAY_BIND=lan ./scripts/docker/setup.sh
```

This builds the OpenClaw image, runs the onboarding flow, and brings up `openclaw-gateway` on port `18789`. The setup script prints the gateway token at the end — save it.

OpenClaw's Control UI requires HTTPS or `localhost` (WebCrypto secure-context check). For remote access, the simplest path is an SSH tunnel from your workstation:

```bash
ssh -L 18789:localhost:18789 -L 3000:localhost:3000 root@<server>
```

Then open `http://localhost:18789` and `http://localhost:3000` in your browser — both are now treated as secure-context localhost.

## 3. Join the networks

Helmdeck ships an overlay file that merges OpenClaw's compose stack onto helmdeck's bridge network:

```bash
docker compose \
  -f /root/openclaw/docker-compose.yml \
  -f /root/helmdeck/deploy/compose/compose.openclaw-sidecar.yml \
  up -d openclaw-gateway
```

After this, `openclaw-gateway` can resolve `helmdeck-control-plane:3000` via DNS.

## 4. Configure helmdeck as an MCP server in OpenClaw

Two paths — pick whichever you prefer:

### 4a. Use the OpenClaw CLI (recommended — schema-validated)

```bash
docker compose -f /root/openclaw/docker-compose.yml run --rm openclaw-cli \
  mcp set helmdeck '{"url":"http://helmdeck-control-plane:3000/api/v1/mcp/sse","headers":{"authorization":"Bearer <your-helmdeck-jwt>"}}'
```

The CLI writes to `~/.openclaw/openclaw.json` and validates the shape against OpenClaw's config schema before saving — preferred over hand-editing because the schema occasionally shifts between OpenClaw releases.

### 4b. Edit `~/.openclaw/openclaw.json` directly (advanced)

OpenClaw stores MCP servers at the **top level** of the config under `mcp.servers`, keyed by server name (NOT under each agent):

```json
{
  "gateway": { "...": "..." },
  "agents":  { "...": "..." },
  "mcp": {
    "servers": {
      "helmdeck": {
        "url": "http://helmdeck-control-plane:3000/api/v1/mcp/sse",
        "headers": {
          "authorization": "Bearer <your-helmdeck-jwt>"
        }
      }
    }
  }
}
```

Then restart `openclaw-gateway` to pick up the change:

```bash
docker compose -f /root/openclaw/docker-compose.yml restart openclaw-gateway
```

### Mint the JWT

```bash
JWT=$(curl -s -X POST http://localhost:3000/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<from install.sh>"}' | jq -r .token)
echo "$JWT"
```

Paste the value into the `Authorization` header above.

## 5. Configure OpenClaw's LLM provider

OpenClaw needs its own LLM credentials. The easiest path is OpenRouter (which is also what helmdeck routes to in the validation walkthrough):

```bash
docker compose -f /root/openclaw/docker-compose.yml run --rm openclaw-cli \
  models auth login openrouter
```

Follow the prompts to paste your OpenRouter API key. Then set the active model:

```bash
docker compose -f /root/openclaw/docker-compose.yml run --rm openclaw-cli \
  models use openrouter/minimax/minimax-m2.7
```

> **Helmdeck-as-LLM-gateway path:** OpenClaw's docs do not clearly document a custom OpenAI-compatible base URL escape hatch as of v0.6.0 of helmdeck. If we confirm via inspection of `models.json` that an arbitrary `base_url` works, this section will gain a "Route OpenClaw's LLM through helmdeck" subsection that points OpenClaw at `http://helmdeck-control-plane:3000/v1/chat/completions` so the T607 success-rate panel lights up from OpenClaw runs. Until then, OpenClaw uses its OpenRouter key directly and helmdeck only sees the MCP tool calls.

## 6. Walk the Phase 5.5 code-edit loop

Open `http://localhost:18789` in your browser, paste the OpenClaw gateway token into Settings, then send a chat prompt:

> Use the helmdeck packs to:
> 1. `repo.fetch` `git@github.com:<me>/<fixture-repo>.git` using vault credential `gh-deploy-key`.
> 2. `fs.list` the clone for `*.md` files.
> 3. `fs.read` the README and propose a one-line edit.
> 4. `fs.patch` to apply the edit (literal search-and-replace).
> 5. `cmd.run` `go test ./...` (or any project check) in the clone.
> 6. `git.commit` with message `chore: helmdeck integration smoke`.
> 7. `repo.push` back to `origin`.

**Pass criteria:**

- The new commit lands on the remote branch.
- The Audit Logs panel in the helmdeck UI (`http://localhost:3000`) shows one entry per pack call, in order.
- The SSH private key never appears in OpenClaw's chat transcript — only the `${vault:gh-deploy-key}` placeholder.

If all three hold, update the status banner at the top of this file to ✅ with today's date + the helmdeck version, and flip the matching row in [`README.md`](README.md).

## Known issue: header key MUST be lowercase `authorization`

> **Status:** Confirmed against OpenClaw 2026.4.10 + `@modelcontextprotocol/sdk@1.29.0` + `eventsource@3.0.7`. Filed upstream as a draft issue at [`docs/integrations/openclaw-upstream-issue.md`](openclaw-upstream-issue.md).

If you write the helmdeck MCP server config with capital-A `Authorization`:

```json
{ "url": "...", "headers": { "Authorization": "Bearer <jwt>" } }
```

…OpenClaw's `bundle-mcp` will fail to connect to helmdeck with:

```
[bundle-mcp] failed to start server "helmdeck" (.../api/v1/mcp/sse): Error: SSE error: Non-200 status code (401)
```

Helmdeck's audit log shows the request as `GET /api/v1/mcp/sse → 401`.

### Why

OpenClaw's `buildSseEventSourceFetch` (`/app/dist/content-blocks-k-DyCOGS.js`) merges the user's `headers` over the SDK's headers as a plain JS object via spread:

```js
return fetchWithUndici(url, {
    ...init,
    headers: { ...sdkHeaders, ...headers }   // sdkHeaders from Headers iteration → lowercase keys
});
```

The MCP SDK returns headers as a `Headers` instance, and iterating it yields **lowercase** keys per the spec — so `sdkHeaders` ends up with `authorization`. When the user config has `Authorization` (capital), the spread produces a plain object with **two distinct keys**:

```js
{ accept: "text/event-stream", authorization: "Bearer <jwt>", Authorization: "Bearer <jwt>" }
```

Undici then constructs a `Headers` list from that object using `append`, which **comma-joins** duplicates (per the Fetch spec) into:

```
Authorization: Bearer <jwt>, Bearer <jwt>
```

Helmdeck's bearer-token parser (and any standards-compliant parser) rejects this malformed header with 401.

### Workaround (until upstream fix)

Use **lowercase `authorization`** as the key in your OpenClaw helmdeck config:

```bash
docker compose -f /root/openclaw/docker-compose.yml run --rm openclaw-cli \
  mcp set helmdeck '{"url":"http://helmdeck-control-plane:3000/api/v1/mcp/sse","headers":{"authorization":"Bearer <jwt>"}}'
```

This makes OpenClaw's spread merge into a single `authorization` entry, which undici then sends as a single well-formed `Authorization` header.

### Upstream fix (proposed)

OpenClaw's `buildSseEventSourceFetch` should construct a `Headers` instance and use `.set()` (which is case-insensitive and replaces) instead of plain-object spread:

```js
function buildSseEventSourceFetch(headers) {
  return (url, init) => {
    const merged = new Headers(init?.headers ?? {});
    for (const [k, v] of Object.entries(headers)) merged.set(k, v);
    return fetchWithUndici(url, { ...init, headers: merged });
  };
}
```

This eliminates the case-collision regardless of how the user wrote the key.

## Troubleshooting

- **`origin not allowed (use HTTPS or localhost secure context)`** — OpenClaw's Control UI requires a secure context. Use the SSH tunnel from step 2, not the public IP.
- **OpenClaw can't reach `helmdeck-control-plane:3000`** — confirm the network overlay is applied: `docker network inspect helmdeck_default` should list `openclaw-gateway` as a member.
- **`401 unauthorized` on every tool call** — JWT expired or wrong scope. Mint a new one and update `~/.openclaw/openclaw.json`.
- **`tools/list` returns nothing** — check that the helmdeck Pack Registry is populated: `curl -H "Authorization: Bearer $JWT" http://localhost:3000/api/v1/packs` should list dozens of packs. If empty, the control plane hasn't registered the built-ins (check `docker compose logs control-plane`).

## References

- [OpenClaw MCP CLI docs](https://docs.openclaw.ai/cli/mcp)
- [OpenClaw Docker install](https://docs.openclaw.ai/install/docker)
- [Helmdeck MCP SSE transport (T302a)](../../internal/api/mcp_sse.go)
