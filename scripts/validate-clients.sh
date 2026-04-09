#!/usr/bin/env bash
# Manual client-validation helper (T564).
#
# Boots the standard Compose stack, mints a JWT, then prints — for each
# supported client — the connect snippet from /api/v1/connect/{client}
# along with a copy-pasteable JSON-RPC scenario for the Phase 5.5
# code-edit loop (repo.fetch -> fs.list -> fs.read -> fs.patch ->
# cmd.run -> git.commit -> repo.push).
#
# This is NOT an automated pass/fail harness. The operator runs the
# scenario by hand against each client and updates the status banner
# in docs/integrations/{client}.md based on what they observe. See
# docs/integrations/README.md for the legend (✅ / 🟡 / ⚪).
#
# Usage:
#   scripts/validate-clients.sh                # boot stack + print all
#   scripts/validate-clients.sh claude-code    # one client only
#   KEEP_STACK=1 scripts/validate-clients.sh   # leave compose running
#
# Env overrides:
#   PORT          control-plane port (default 3000)
#   FIXTURE_REPO  git URL the scenario should target (default placeholder)
#   VAULT_KEY     vault credential name for the SSH key (default gh-deploy-key)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/deploy/compose/compose.yaml"
ENV_FILE="${REPO_ROOT}/deploy/compose/.env.validate-clients"
PORT="${PORT:-3000}"
FIXTURE_REPO="${FIXTURE_REPO:-git@github.com:<you>/<fixture-repo>.git}"
VAULT_KEY="${VAULT_KEY:-gh-deploy-key}"

CLIENTS=(claude-code claude-desktop openclaw gemini-cli)
if [[ $# -gt 0 ]]; then
  CLIENTS=("$@")
fi

cleanup() {
  if [[ "${KEEP_STACK:-0}" == "1" ]]; then
    echo
    echo "[validate-clients] KEEP_STACK=1 — leaving compose stack running on :${PORT}" >&2
    return
  fi
  docker compose -f "${COMPOSE_FILE}" --env-file "${ENV_FILE}" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Reuse the smoke env file if present, otherwise stub one.
if [[ ! -f "${ENV_FILE}" ]]; then
  cp "${REPO_ROOT}/deploy/compose/.env.example" "${ENV_FILE}"
fi

echo "[validate-clients] booting compose stack..." >&2
docker compose -f "${COMPOSE_FILE}" --env-file "${ENV_FILE}" up -d >/dev/null

echo "[validate-clients] waiting for control-plane on :${PORT}..." >&2
for _ in $(seq 1 30); do
  if curl -fsS "http://localhost:${PORT}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

# Mint a short-lived JWT via the dev login endpoint.
TOKEN="${HELMDECK_TOKEN:-$(curl -fsS -X POST "http://localhost:${PORT}/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' | jq -r .token)}"

if [[ -z "${TOKEN}" || "${TOKEN}" == "null" ]]; then
  echo "[validate-clients] failed to obtain JWT" >&2
  exit 1
fi

print_scenario() {
  cat <<EOF

  Phase 5.5 code-edit-loop scenario (paste this into the client as a prompt):
  ----------------------------------------------------------------------------
  Use the helmdeck packs to perform the following loop, in order, against
  the repository ${FIXTURE_REPO} using vault credential ${VAULT_KEY}:

    1. repo.fetch  — clone the repo into a fresh workspace
    2. fs.list     — list *.md files under the clone
    3. fs.read     — read the README
    4. fs.patch    — apply a one-line literal edit to the README
    5. cmd.run     — run the project's test or lint command in the clone
    6. git.commit  — stage + commit with message
                     "chore: helmdeck integration smoke"
    7. repo.push   — push the commit back to origin

  After the loop completes, verify by hand:
    [ ] the new commit lands on the remote branch
    [ ] every step appears in the audit log (UI: Audit Logs panel)
    [ ] the SSH private key never appears in the client transcript;
        only the \${vault:${VAULT_KEY}} placeholder
  ----------------------------------------------------------------------------
EOF
}

for client in "${CLIENTS[@]}"; do
  echo
  echo "================================================================"
  echo "  Client: ${client}"
  echo "================================================================"
  snippet="$(curl -fsS "http://localhost:${PORT}/api/v1/connect/${client}?token=${TOKEN}" || true)"
  if [[ -z "${snippet}" ]]; then
    echo "  (no connect snippet — endpoint not implemented for ${client})"
    echo "  See docs/integrations/${client}.md for the TODO list."
    continue
  fi
  echo
  echo "  Connect snippet (merge into the file at .install_path):"
  echo "${snippet}" | jq .
  print_scenario
  echo
  echo "  Setup guide: docs/integrations/${client}.md"
done

echo
echo "[validate-clients] done. Update docs/integrations/{client}.md status banners based on observed results."
