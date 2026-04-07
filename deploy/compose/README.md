# Helmdeck — Docker Compose deployment

The Compose stack is the **dev / single-node tier** (ADR 009). It brings up the control plane, mounts the host Docker socket so the control plane can spawn ephemeral browser-sidecar containers, and persists the SQLite database to a named volume.

For production multi-node deployments, use the Helm chart in `charts/baas-platform/` (lands in T702).

## Quick start

```bash
cp deploy/compose/.env.example deploy/compose/.env
# edit deploy/compose/.env and set HELMDECK_JWT_SECRET to `openssl rand -hex 32`

docker compose -f deploy/compose/compose.yaml --env-file deploy/compose/.env up -d
docker compose -f deploy/compose/compose.yaml --env-file deploy/compose/.env logs -f control-plane
```

The control plane is reachable on `http://127.0.0.1:3000`. The port is bound to the loopback interface only — front it with a reverse proxy (Caddy / nginx / Traefik) for external access.

## Mint a bootstrap token

```bash
docker compose -f deploy/compose/compose.yaml exec control-plane \
  /usr/local/bin/control-plane \
    -mint-token=admin \
    -mint-token-name="Operator" \
    -mint-token-client=cli \
    -mint-token-scopes=admin \
    -mint-token-ttl=720h
```

Copy the printed JWT into your client (Claude Code, Claude Desktop, OpenClaw, Gemini CLI, or `curl -H "Authorization: Bearer …"`).

## End-to-end smoke

```bash
make smoke
```

The smoke harness boots the stack, mints a token, creates a session against `helmdeck-sidecar:latest`, navigates to a tiny inline document, captures a screenshot, asserts on PNG magic bytes, terminates the session, and tears down the stack.

## What the stack runs

| Service | Image | Purpose |
| :--- | :--- | :--- |
| `control-plane` | `ghcr.io/tosin2013/helmdeck:dev` (built from this repo) | REST API, session manager, audit log writer, JWT issuer, future AI gateway / MCP registry / vault |
| `sidecar-warm` | `docker:cli` (one-shot) | Pre-pulls `helmdeck-sidecar:latest` so the first session is warm |
| _ephemeral browser sessions_ | `ghcr.io/tosin2013/helmdeck-sidecar:latest` | Spawned dynamically by the control plane on the `baas-net` bridge — not declared in `compose.yaml` |

## Volumes and networks

- **`helmdeck-data`** — named volume holding `/data/helmdeck.db` (SQLite). Survives `docker compose down`; remove with `docker compose down -v` or `docker volume rm helmdeck-data`.
- **`baas-net`** — bridge network the control plane attaches every spawned browser session to. Internal to the host; the only published port is `3000` on the loopback interface.

## Security notes

- The Docker socket mount gives the control plane root-equivalent access to the host. Treat the host as a trust boundary; do not run untrusted control-plane builds.
- `HELMDECK_JWT_SECRET` must be ≥32 bytes (64 hex chars). The control plane refuses shorter values.
- Tokens minted by `-mint-token` survive control-plane restarts because the secret is in the env file. If you lose the secret, every existing token becomes invalid.
- Browser sessions inherit the runtime hardening from `internal/session/docker/runtime.go`: `cap_drop=ALL`, `cap_add=SYS_ADMIN`, `no-new-privileges`. The kernel sandbox is provided by Docker (standard tier per ADR 011); switch to gVisor or Firecracker for higher isolation in production.

## Troubleshooting

| Symptom | Likely cause | Fix |
| :--- | :--- | :--- |
| `control-plane` container restart-loops with "secret must be ≥32 bytes" | `HELMDECK_JWT_SECRET` empty or too short | Generate with `openssl rand -hex 32` and update `.env` |
| Session creation returns 500 with "permission denied … docker.sock" | Host docker socket isn't readable by the container's UID | On non-root hosts add `group_add: ["docker"]` to the service |
| Browser screenshot returns garbage / 502 | Sidecar image wasn't pulled before first session | `docker pull ghcr.io/tosin2013/helmdeck-sidecar:latest` then retry |
| `baas-net` already exists | Stale stack from a previous tag | `docker network rm baas-net` |
