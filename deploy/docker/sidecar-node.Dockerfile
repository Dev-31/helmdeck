# Helmdeck Node.js sidecar — see ADR 001, docs/SIDECAR-LANGUAGES.md.
#
# Extends the base sidecar with a Node.js LTS toolchain so the
# node.run pack can execute snippets, run npm/yarn/pnpm scripts,
# lint with eslint, format with prettier, and install dependencies.
#
# Tag: ghcr.io/tosin2013/helmdeck-sidecar-node:<release>
#
# Built by `make sidecar-node-build`. Operators who need a different
# Node version or a specific package manager pinned should fork this
# Dockerfile per docs/SIDECAR-EXTENDING.md.

ARG BASE_IMAGE=ghcr.io/tosin2013/helmdeck-sidecar:latest
FROM ${BASE_IMAGE}

USER root

# Node 20 LTS via NodeSource. Debian's packaged node is several
# majors behind; NodeSource is the standard upstream for current
# LTS lines.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg \
 && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

# pnpm + yarn alongside the bundled npm so package.json scripts work
# regardless of which package manager the cloned repo uses. corepack
# ships with node and is the official path for managing pnpm/yarn
# without polluting the global npm namespace.
RUN corepack enable \
 && corepack prepare pnpm@latest --activate \
 && corepack prepare yarn@stable --activate

# Common dev tools installed globally so the LLM doesn't have to
# install them per session.
RUN npm install -g --no-fund --no-audit \
      typescript \
      ts-node \
      eslint \
      prettier \
      vitest

USER helmdeck
WORKDIR /home/helmdeck

# Inherits ENTRYPOINT, CHROMIUM_PORT, HELMDECK_MODE, EXPOSE from the
# base image.
