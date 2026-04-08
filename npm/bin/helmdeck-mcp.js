#!/usr/bin/env node
// Launcher shim for @helmdeck/mcp-bridge.
//
// npm sets up the `helmdeck-mcp` bin symlink at install time, *before*
// postinstall runs, so this file must exist in the published tarball.
// At runtime it execs the platform-native binary that postinstall
// downloaded into the same directory; if the binary is missing it
// prints a clear remediation hint instead of a cryptic spawn error.

"use strict";

const path = require("path");
const fs = require("fs");
const { spawnSync } = require("child_process");

const binName = process.platform === "win32" ? "helmdeck-mcp.exe" : "helmdeck-mcp";
const binPath = path.join(__dirname, binName);

if (!fs.existsSync(binPath)) {
  console.error(
    `[helmdeck-mcp] native binary not found at ${binPath}.\n` +
    `This usually means the postinstall step was skipped (HELMDECK_MCP_SKIP_DOWNLOAD=1)\n` +
    `or the download failed. Re-run:\n\n` +
    `    npm rebuild @helmdeck/mcp-bridge\n`
  );
  process.exit(127);
}

const result = spawnSync(binPath, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`[helmdeck-mcp] failed to spawn ${binPath}: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status === null ? 1 : result.status);
