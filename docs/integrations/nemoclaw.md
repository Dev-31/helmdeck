# NemoClaw

> **Status:** 🟡 Documented, not yet verified end-to-end
> NemoClaw is **not** a separate Helmdeck connect target. It is NVIDIA's sandboxed runtime *around* OpenClaw — running OpenClaw inside an OpenShell container with Landlock/seccomp/netns guardrails — and reuses OpenClaw's MCP config schema inside the sandbox. This page is a thin wrapper around [openclaw.md](openclaw.md) with the sandbox-specific notes. Promote to ✅ once a maintainer has walked the Phase 5.5 loop inside a NemoClaw sandbox.

## Prerequisites

Same as [openclaw.md](openclaw.md), plus:

- NemoClaw installed and a sandbox provisioned per the [NVIDIA NemoClaw quickstart](https://docs.nvidia.com/nemoclaw/latest/get-started/quickstart.html).
- The Helmdeck control plane reachable from inside the sandbox network namespace (NemoClaw uses `netns` to restrict egress — you may need to allowlist the control-plane host/port in the sandbox network policy).
- The `helmdeck-mcp` binary either baked into the sandbox image, mounted into the container, or installed at sandbox bootstrap time. The agent inside the sandbox can only execute binaries it can see — Landlock restricts writes to `/sandbox` and `/tmp`, so place the bridge accordingly.

## 1. Install the bridge

See [claude-code.md §1](claude-code.md#1-install-the-bridge) for distribution channels. The wrinkle is **where** to install it: it must be reachable on `PATH` from inside the NemoClaw sandbox. Recommended approaches:

- Mount the host's `helmdeck-mcp` into the sandbox via the NemoClaw blueprint, or
- Add `go install github.com/tosin2013/helmdeck/cmd/helmdeck-mcp@latest` to the sandbox bootstrap script.

## 2. Fetch the connect snippet

NemoClaw is not its own client in `/api/v1/connect/`. Use the OpenClaw snippet — it is the schema NemoClaw reads inside the sandbox:

```bash
curl -s "http://localhost:3000/api/v1/connect/openclaw?token=$HELMDECK_TOKEN" | jq .
```

## 3. Configure NemoClaw

Inside the sandbox, write the OpenClaw config to `<sandbox>/.openclaw/openclaw.json` using the JSON shape from [openclaw.md §3](openclaw.md#3-configure-openclaw). Two NemoClaw-specific notes:

- The path is **inside** the sandbox filesystem — typically `/sandbox/.openclaw/openclaw.json` if you're using the default `/sandbox` writable mount.
- `HELMDECK_URL` must be reachable from the sandbox's network namespace, not the host's. If you're running Helmdeck on the host at `http://localhost:3000`, point at the host's bridge IP (e.g. `http://172.17.0.1:3000` for default Docker bridge networking) instead of `localhost`.

## 4. Walk the Phase 5.5 code-edit loop

Identical to [claude-code.md §4](claude-code.md#4-walk-the-phase-55-code-edit-loop), but driven from the agent running **inside** the NemoClaw sandbox. Pay extra attention to the SSH-key assertion: NemoClaw's whole point is keeping secrets out of the model context, so a leak here would be a real audit finding.

## Troubleshooting

- **`helmdeck-mcp: command not found` in the sandbox** — the binary is on the host but not visible inside the sandbox. Mount it or install it during sandbox bootstrap.
- **`connection refused` to the control plane** — Landlock/netns is blocking egress. Allowlist the control-plane host in the sandbox network policy and re-test with `curl http://<host>:3000/healthz` from inside the sandbox.
- See [openclaw.md §Troubleshooting](openclaw.md#troubleshooting) for OpenClaw-layer issues.

References:
- [NVIDIA/NemoClaw GitHub](https://github.com/NVIDIA/NemoClaw)
- [NVIDIA NemoClaw Quickstart](https://docs.nvidia.com/nemoclaw/latest/get-started/quickstart.html)
- [NVIDIA NemoClaw product page](https://www.nvidia.com/en-us/ai/nemoclaw/)
