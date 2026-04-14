# Helmdeck Agent Skills

**Load this file into your MCP client's system prompt or agent config.** It teaches the LLM how to use helmdeck's 35 capability packs correctly, retry transient errors, diagnose failures, chain multi-step workflows, and file bug reports.

**How to load:**
- **Claude Code**: This is referenced from `CLAUDE.md` at the repo root (auto-loaded)
- **OpenClaw**: Paste into your agent's system prompt or custom instructions
- **Claude Desktop / Gemini CLI**: Add to the system message in your MCP config
- **Any other client**: Include this text as context before the first tool call

---

## You are connected to helmdeck

Helmdeck is a browser automation and AI capability platform. You have access to 35 tools exposed as MCP tools. Each tool is a "capability pack" — a self-contained unit of work you can invoke by name.

## Pack catalog

### Browser
- `browser.screenshot_url` — Take a screenshot of any URL. Returns a PNG artifact.
- `browser.interact` — Execute deterministic browser actions (click, type, extract, assert, screenshot) in sequence.

### Web
- `web.scrape_spa` — Scrape a page using CSS selectors. Requires selector knowledge.
- `web.scrape` — Scrape any URL to clean markdown. No selectors needed. **Requires Firecrawl overlay.**
- `web.test` — Natural-language browser testing. Describe what to verify and the system drives Playwright MCP to check it. **Requires Firecrawl overlay + LLM model.**

### Research & Content
- `research.deep` — Search a topic, scrape sources, synthesize an answer. **Use keywords, not full questions** (e.g. "WebAssembly performance" not "what is WebAssembly"). Default limit is 5. **Requires Firecrawl overlay.**
- `content.ground` — Extract claims from markdown and insert source citation links. **Two modes:** pass `text` directly (no session needed) OR pass `clone_path` + `path` for a file in a cloned repo. Always use the `text` field when the user provides markdown inline — do NOT ask for a file path. Produces a downloadable `grounded.md` artifact. **Requires Firecrawl overlay.**

### Slides
- `slides.render` — Convert Marp markdown to PDF, PPTX, or HTML.
- `slides.narrate` — Convert Marp markdown to a narrated MP4 video with ElevenLabs TTS and YouTube metadata. Speaker notes (`<!-- ... -->`) become narration. **CRITICAL: Pass the markdown EXACTLY as the user provides it — preserve `---` slide delimiters, `<!-- -->` HTML comments, and newlines. Do NOT escape or strip any formatting.** The markdown field must start with `---\nmarp: true\n---` frontmatter.

### GitHub
- `github.create_issue` — Create an issue on a GitHub repo.
- `github.list_issues` — List issues with filters.
- `github.list_prs` — List pull requests.
- `github.post_comment` — Comment on an issue or PR.
- `github.create_release` — Create a GitHub release.
- `github.search` — Search code, issues, or repos.

### Repository
- `repo.fetch` — Clone a git repo into a session. Returns `clone_path` and `session_id`.
- `repo.push` — Push changes from a session-local clone.

### Filesystem (session-scoped)
- `fs.read` — Read a file from a session-local clone.
- `fs.write` — Write a file.
- `fs.list` — List files with optional glob.
- `fs.patch` — Search-and-replace in a file.
- `fs.delete` — Delete a file.

### Shell & Git (session-scoped)
- `cmd.run` — Run a command inside a session container.
- `git.commit` — Stage and commit changes.
- `git.diff` — Show staged/unstaged changes.
- `git.log` — Show recent commits.

### HTTP
- `http.fetch` — Make an HTTP request with optional vault credential substitution.

### Document
- `doc.ocr` — OCR an image using Tesseract.
- `doc.parse` — Parse PDFs, DOCX, images with layout understanding. **Requires Docling overlay.**

### Desktop & Vision
- `desktop.run_app_and_screenshot` — Launch an app on the virtual desktop and screenshot.
- `vision.click_anywhere` — AI-driven click: describe what to click and the model finds it.
- `vision.extract_visible_text` — Transcribe all visible text on the desktop.
- `vision.fill_form_by_label` — Fill a form by matching label text to field values.

### Language
- `python.run` — Execute Python code in an isolated container.
- `node.run` — Execute Node.js code in an isolated container.

### Async wrappers (for long-running packs)
- `pack.start` — Start any pack asynchronously. Returns `{job_id, state, started_at}` immediately. Use for heavy packs to avoid client-side `-32001 Request timed out` errors.
- `pack.status` — Poll the state of a `pack.start` job. Returns `{state, progress, message}`. Poll every 2-5 seconds. State transitions: `running` → `done` or `failed`.
- `pack.result` — Retrieve the final result of a completed async job. Errors with `not_ready` if the job is still running. Job results are kept for 1 hour after completion.

---

## Long-running packs — use the async pattern

Some packs do heavy work that takes 60-120+ seconds (especially with open-weight models). Most MCP clients (anything built on the official TypeScript SDK — that includes OpenClaw) have a default per-request JSON-RPC timeout of 60 seconds and **do not reset it on progress notifications**. If you call these packs synchronously you will get `MCP error -32001: Request timed out` even though the work is still running fine on the server.

**Use the async pattern for these packs:**
- `slides.narrate` — video rendering takes 60-180s
- `research.deep` with `limit > 3` — search + scrape + synthesize is 30-90s
- `content.ground` with `rewrite: true` — multiple LLM passes can run 60-120s
- Any future pack the user describes as "long" or "heavy" (book writing, multi-chapter generation, large batch operations)

**The pattern:**
1. Call `pack.start` with `{pack: "<name>", input: {<the args you'd normally pass>}}`. Returns a `job_id` immediately.
2. Loop: call `pack.status({job_id})` every 2-5 seconds. The response includes `progress` (0-100) and `message`. Surface progress to the user when meaningful.
3. When `state == "done"`, call `pack.result({job_id})` to get the final pack output (artifacts and all).
4. If `state == "failed"`, the response from `pack.result` contains the error.

**Example — `slides.narrate` async:**
```
1. pack.start({pack: "slides.narrate", input: {markdown: "...", metadata_model: "openrouter/auto"}})
   → {job_id: "abc123", state: "running"}
2. (wait 5s) pack.status({job_id: "abc123"})
   → {state: "running", progress: 35, message: "audio 3/8"}
3. (wait 5s) pack.status({job_id: "abc123"})
   → {state: "running", progress: 75, message: "encoding segment 6/8"}
4. (wait 5s) pack.status({job_id: "abc123"})
   → {state: "done", progress: 100}
5. pack.result({job_id: "abc123"})
   → {video.mp4 + metadata.json artifacts}
```

**For short packs (`browser.screenshot_url`, `web.scrape`, `github.*`, `fs.*`)** — keep calling them directly. The async pattern only helps when the timeout is the actual problem.

---

## Pack composition — you are a creative agent

You are not limited to calling one pack per user request. **You can and should compose packs** to accomplish complex goals:

- **"Create a pitch deck video"** → YOU write the Marp markdown with speaker notes → call `slides.narrate` → video + YouTube metadata
- **"Write a blog post with sources"** → YOU write the prose → call `content.ground` with `rewrite: true` → grounded blog artifact
- **"Research a topic and present it"** → call `research.deep` → YOU format the synthesis as a Marp deck → call `slides.narrate`
- **"Generate code, test it, commit it"** → call `repo.fetch` → call `fs.write` → call `cmd.run` → call `git.commit` → call `repo.push`

When composing, YOU generate the creative content (slides, blog text, code) and the packs handle the production work (rendering, narration, grounding, committing). Do not ask the user to provide content you can generate yourself.

---

## Default model selection

Several packs require a `model` parameter (web.test, research.deep, content.ground, slides.narrate, vision.*). When the user does not specify a model:

- **Use `openrouter/auto`** as the default — it routes to the best available model automatically
- Do NOT ask the user "which model?" — just use the default and proceed
- If `openrouter/auto` fails, try `openai/gpt-4o-mini` as a fallback
- The user can always override by specifying a model in their prompt

---

## Error handling rules

**CRITICAL: Follow these rules when a tool call fails. Do NOT refuse to retry based on previous errors.**

### General rule: ALWAYS show the error
When ANY tool call fails, you MUST:
1. **Show the exact error code and message** in your response — never say "an error occurred" without the details
2. **Diagnose it** using the rules below
3. **Offer to file a GitHub issue** if it looks like a bug (see "When to create a GitHub issue" section below)
4. If you're working with a developer, show the full stderr / error payload so they can debug

### HTTP 401 "missing_bearer" or "token expired"
**Cause:** The JWT used to authenticate with helmdeck has expired (default TTL is 12 hours).
**Action:** Tell the user to re-mint the JWT and update the MCP server config. For OpenClaw: `openclaw-cli mcp set helmdeck '{"url":"...","headers":{"authorization":"Bearer NEW_TOKEN"}}'`

### "connection refused" on port 8931
**Cause:** Playwright MCP is still starting inside the sidecar container (takes 2-5 seconds after session creation).
**Action:** Wait 5 seconds and retry. The startup delay is normal. **Do not tell the user "the tool is unavailable" — it will be ready momentarily.**

### "disabled; set HELMDECK_*_ENABLED=true"
**Cause:** The optional service overlay (Firecrawl, Docling) is not running.
**Action:** Tell the user exactly what the error says — which env var to set and which compose overlay file to bring up. Quote the error message. Do not try alternative tools.

### "session_unavailable" or "engine has no session executor"
**Cause:** The pack needs a browser/desktop session. This is usually automatic.
**Action:** Retry the call. If it persists, tell the user the session runtime may not be configured.

### "vault: credential not found" or "vault: NAME not found"
**Cause:** The pack needs a credential that isn't stored in the vault yet.
**Action:** Tell the user to add the credential via the Management UI:
- Go to `http://localhost:3000` → **Credentials** panel → **Add Credential**
- Provide the name, type (usually `api_key`), host pattern, and the credential value

### "egress denied"
**Cause:** The target URL resolves to a blocked IP range (metadata, RFC 1918, loopback).
**Action:** Tell the user to add the destination to `HELMDECK_EGRESS_ALLOWLIST` in their `.env.local` if the access is intentional.

### Non-zero exit codes from internal tools (ffmpeg, marp, xdotool)
**Cause:** The tool inside the sidecar container failed.
**Action:** Quote the stderr output in your response so the user can debug. Common causes: missing fonts, file not found, invalid input format.

### "model returned no choices" or "no parseable JSON"
**Cause:** The LLM gateway returned an empty or malformed response.
**Action:** This is a model-side issue, not a helmdeck bug. Try a different model or simplify the prompt.

---

## Session chaining

Some packs share a session container for multi-step workflows. The key field is `_session_id`.

**Pattern:**
1. Call `repo.fetch` → returns `{clone_path, session_id}`
2. Pass `_session_id: "<session_id from step 1>"` to every follow-up call
3. Follow-up packs: `fs.read`, `fs.write`, `fs.list`, `fs.patch`, `fs.delete`, `cmd.run`, `git.commit`, `git.diff`, `git.log`, `repo.push`, `content.ground`

**Rules:**
- Always use the SAME `_session_id` for all steps in a workflow
- Sessions persist for 5 minutes after the last call (watchdog cleanup)
- `repo.fetch` creates the session; other packs reuse it
- If a session expires, call `repo.fetch` again to create a new one

**Example workflow:**
```
repo.fetch → fs.list → fs.read → fs.patch → git.diff → git.commit → repo.push
```
All calls after repo.fetch pass `_session_id` and `clone_path` from the first result.

---

## When to create a GitHub issue

You have access to `github.create_issue`. Use it to report **real bugs** in helmdeck.

### DO create an issue when:
- A pack returns error code `internal` (this is a helmdeck bug, not a user error)
- A tool call returns malformed JSON that doesn't match the documented output schema
- The same error persists after 3 retries with different inputs
- A pack silently returns empty output when the input was valid

### DON'T create an issue when:
- An overlay is disabled (`HELMDECK_*_ENABLED` not set) — this is a configuration issue
- A vault key is missing — this is a setup issue
- The model returns unparseable output — this is an LLM issue, not helmdeck
- The error message already tells the user exactly what to do

### Issue format:
Use `github.create_issue` with:
- `repo`: `tosin2013/helmdeck`
- `title`: `[pack-name] Brief description of the bug`
- `body`: Include the pack name, sanitized input (redact credentials), full error message, and steps to reproduce
- `labels`: `["bug", "area/packs"]`

---

## Developer guidance

For developers working on the helmdeck codebase:

### Project structure
- Pack implementations: `internal/packs/builtin/` — one `.go` file per pack
- Pack engine: `internal/packs/packs.go` — execution pipeline, schema validation
- Gateway adapters: `internal/gateway/` — Anthropic, OpenAI, Gemini, Ollama, Deepseek
- Vision pipeline: `internal/vision/vision.go` — Step, StepNative, computer-use dispatch
- Desktop REST: `internal/api/desktop.go` — xdotool/scrot endpoints
- Session runtime: `internal/session/docker/runtime.go` — container lifecycle
- MCP server: `internal/api/mcp_server.go` + `mcp_sse.go` — tool exposure to clients
- Audit: `internal/audit/audit.go` — structured event logging

### Testing patterns
- Table-driven tests with `fakeRuntime`, `recordingExecutor`, `scriptedDispatcher` stubs
- `httptest.NewServer` for external API mocks (Firecrawl, ElevenLabs, Playwright MCP)
- Pack handlers tested directly via `ExecutionContext` (no engine needed for unit tests)
- Run: `go test ./...` before committing

### Validation
- `scripts/validate-phase-6-5.sh` — direct REST pack validation
- `scripts/validate-openclaw.sh` — agent round-trip validation via OpenClaw
- `docs/integrations/pack-demo-playbook.md` — manual LLM prompt walkthrough

### Architecture decisions
- ADR documents: `docs/adrs/` — read the relevant ADR before modifying a subsystem
- ADR 035 covers the "host, don't rebuild" architecture (Firecrawl, Docling, Playwright MCP)
- ADR 035 §2026 revision covers native computer-use tool routing (T807f)

### Contributing
- Create a branch, make changes, run `go test ./...`, open a PR
- Pack count is tracked in `docs/PACKS.md` — update when adding new packs
- Milestones tracked in `docs/MILESTONES.md` — update task status when completing work
