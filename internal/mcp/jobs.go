// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The helmdeck contributors

package mcp

// jobs.go — async job registry for long-running pack calls.
//
// Why this exists: the MCP TypeScript SDK (which OpenClaw and most
// JS-based MCP clients are built on) defaults to a 60-second
// per-request JSON-RPC timeout AND defaults `resetTimeoutOnProgress`
// to false (issue #245, PR #849 rejected Sep 2025). That means even
// a perfectly spec-compliant `notifications/progress` stream from
// our server will not save heavy packs (slides.narrate,
// research.deep, content.ground rewrite, future book-writing
// workflows) from MCP error -32001 "Request timed out" on those
// clients.
//
// The fix is the pattern OpenClaw uses for its own long-running
// tools: split the call in two. `pack.start` returns a job_id
// immediately (well within any reasonable timeout), the work runs
// in a background goroutine, and the client polls `pack.status` —
// each poll is a tiny new request with its own fresh timeout — until
// state == "done", then `pack.result` retrieves the final payload.
//
// SKILLS.md teaches the agent: prefer the async path for known-heavy
// packs. The sync path stays available for clients that handle long
// calls fine (Claude Desktop, MCP Inspector with --reset-on-progress,
// Python-SDK clients).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/tosin2013/helmdeck/internal/packs"
)

// jobTTL is how long a finished job's result stays in the registry
// before the sweeper evicts it. 1 hour leaves comfortable headroom
// for an agent that fires `pack.start` and then walks away to do
// other thinking before polling `pack.result`.
const jobTTL = 1 * time.Hour

// jobSweepInterval is how often the background goroutine scans the
// registry for expired jobs. Doesn't need to be precise.
const jobSweepInterval = 5 * time.Minute

// asyncJob is one in-flight or completed pack execution. The mutex
// guards the mutable fields (state, progress, message, result, err,
// endedAt) — startedAt and the immutable identity fields are safe
// to read without it.
type asyncJob struct {
	ID        string
	Pack      string
	StartedAt time.Time

	mu       sync.Mutex
	state    string // "running" | "done" | "failed"
	progress float64
	message  string
	result   *packs.Result
	err      error
	endedAt  time.Time
	cancel   context.CancelFunc
}

// snapshot is what `pack.status` returns. Pulled atomically under
// the job mutex so a polling client never sees a partial update.
type jobSnapshot struct {
	JobID     string  `json:"job_id"`
	Pack      string  `json:"pack"`
	State     string  `json:"state"`
	Progress  float64 `json:"progress"`
	Message   string  `json:"message,omitempty"`
	StartedAt string  `json:"started_at"`
	EndedAt   string  `json:"ended_at,omitempty"`
	Error     string  `json:"error,omitempty"`
}

func (j *asyncJob) snapshot() jobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := jobSnapshot{
		JobID:     j.ID,
		Pack:      j.Pack,
		State:     j.state,
		Progress:  j.progress,
		Message:   j.message,
		StartedAt: j.StartedAt.UTC().Format(time.RFC3339),
	}
	if !j.endedAt.IsZero() {
		out.EndedAt = j.endedAt.UTC().Format(time.RFC3339)
	}
	if j.err != nil {
		out.Error = j.err.Error()
	}
	return out
}

// jobRegistry holds active and recently-completed async jobs. The
// sweeper goroutine evicts entries older than jobTTL after they
// reach a terminal state.
type jobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*asyncJob
}

func newJobRegistry() *jobRegistry {
	r := &jobRegistry{jobs: make(map[string]*asyncJob)}
	go r.sweepLoop(context.Background())
	return r
}

func (r *jobRegistry) put(j *asyncJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.ID] = j
}

func (r *jobRegistry) get(id string) (*asyncJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	return j, ok
}

func (r *jobRegistry) drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, id)
}

// sweepLoop runs forever, scanning every jobSweepInterval and
// dropping terminal jobs whose endedAt is older than jobTTL.
// Process-scoped context — the registry lives for the life of the
// PackServer, which is process-wide. No shutdown path needed.
func (r *jobRegistry) sweepLoop(ctx context.Context) {
	t := time.NewTicker(jobSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(time.Now())
		}
	}
}

func (r *jobRegistry) sweep(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, j := range r.jobs {
		j.mu.Lock()
		expired := !j.endedAt.IsZero() && now.Sub(j.endedAt) > jobTTL
		j.mu.Unlock()
		if expired {
			delete(r.jobs, id)
		}
	}
}

// startAsync spawns a goroutine that runs engine.Execute for pack
// with input, capturing progress into the job. The returned job is
// already registered and in state "running"; callers should not
// touch it directly — go through the registry methods.
//
// The goroutine uses a detached context (context.Background) because
// the SSE/WS request that triggered pack.start may close before the
// pack finishes — that's the whole point of the async pattern. We
// keep the cancel handle on the job for a future `pack.cancel` tool.
func (s *PackServer) startAsync(pack *packs.Pack, input json.RawMessage) *asyncJob {
	jobCtx, cancel := context.WithCancel(context.Background())
	j := &asyncJob{
		ID:        newJobID(),
		Pack:      pack.Name,
		StartedAt: time.Now().UTC(),
		state:     "running",
		cancel:    cancel,
	}
	s.jobs.put(j)

	progress := func(pct float64, message string) {
		j.mu.Lock()
		j.progress = pct
		if message != "" {
			j.message = message
		}
		j.mu.Unlock()
	}
	jobCtx = packs.WithProgress(jobCtx, progress)

	go func() {
		defer cancel()
		res, err := s.engine.Execute(jobCtx, pack, input)
		j.mu.Lock()
		j.endedAt = time.Now().UTC()
		if err != nil {
			j.state = "failed"
			j.err = err
		} else {
			j.state = "done"
			j.result = res
			j.progress = 100
		}
		j.mu.Unlock()
	}()

	return j
}

// newJobID returns a short random identifier for a job. 128 bits is
// plenty — collision probability is negligible at any realistic
// concurrent-job count.
func newJobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// asyncPackTools are the three MCP tools exposed by the async layer.
// They show up in tools/list alongside every regular pack so the
// LLM can discover them. Schemas are intentionally permissive on the
// `input` field — the async layer is a thin wrapper, the wrapped
// pack's own schema is what ultimately validates the payload.
func asyncPackTools() []Tool {
	startSchema := mustJSON(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pack":  map[string]any{"type": "string", "description": "Pack name to run asynchronously, e.g. \"slides.narrate\""},
			"input": map[string]any{"type": "object", "description": "Arguments object that would normally be passed directly to the pack."},
		},
		"required": []string{"pack"},
	})
	idSchema := mustJSON(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"job_id": map[string]any{"type": "string"},
		},
		"required": []string{"job_id"},
	})
	return []Tool{
		{
			Name:        "pack.start",
			Description: "Start a pack call asynchronously and return a job_id immediately. Use for heavy packs (slides.narrate, research.deep, content.ground with rewrite=true) when your MCP client has a low per-request timeout. Then poll pack.status, then call pack.result.",
			InputSchema: startSchema,
		},
		{
			Name:        "pack.status",
			Description: "Check the state of an async pack call. Returns {state: running|done|failed, progress: 0-100, message}. Poll every 2-5 seconds. When state is done, call pack.result.",
			InputSchema: idSchema,
		},
		{
			Name:        "pack.result",
			Description: "Retrieve the final result of a completed async pack call. Errors if the job is still running. Job results are kept for 1 hour after completion.",
			InputSchema: idSchema,
		},
	}
}

// dispatchAsyncTool handles pack.start / pack.status / pack.result
// without going through the engine. Returns (toolResult, true) when
// the call was an async-tool call and was handled; (nil, false) when
// the caller should fall through to the regular pack path.
func (s *PackServer) dispatchAsyncTool(name string, arguments json.RawMessage) (map[string]any, bool) {
	switch name {
	case "pack.start":
		var args struct {
			Pack  string          `json:"pack"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(arguments, &args); err != nil {
			return errorToolResult("invalid_input", "pack.start: "+err.Error()), true
		}
		if args.Pack == "" {
			return errorToolResult("invalid_input", "pack.start: pack is required"), true
		}
		pack, err := s.registry.Get(args.Pack, "")
		if err != nil {
			return errorToolResult("unknown_pack", "pack.start: unknown pack "+args.Pack), true
		}
		input := args.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		j := s.startAsync(pack, input)
		body, _ := json.Marshal(j.snapshot())
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(body)}},
			"isError": false,
		}, true

	case "pack.status":
		j, ok := s.lookupJob(arguments)
		if !ok {
			return errorToolResult("unknown_job", "pack.status: job_id not found"), true
		}
		body, _ := json.Marshal(j.snapshot())
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(body)}},
			"isError": false,
		}, true

	case "pack.result":
		j, ok := s.lookupJob(arguments)
		if !ok {
			return errorToolResult("unknown_job", "pack.result: job_id not found"), true
		}
		j.mu.Lock()
		state := j.state
		res := j.result
		jobErr := j.err
		j.mu.Unlock()
		switch state {
		case "running":
			return errorToolResult("not_ready", fmt.Sprintf("pack.result: job %s still running — keep polling pack.status", j.ID)), true
		case "failed":
			return packErrorAsToolResult(jobErr), true
		case "done":
			s.jobs.drop(j.ID)
			return s.packResultAsToolResult(context.Background(), res), true
		default:
			return errorToolResult("internal", "pack.result: unexpected job state "+state), true
		}
	}
	return nil, false
}

// lookupJob extracts the job_id from a tool-call arguments blob and
// returns the matching job. Both pack.status and pack.result use the
// same shape, so the parsing is shared.
func (s *PackServer) lookupJob(arguments json.RawMessage) (*asyncJob, bool) {
	var args struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil || args.JobID == "" {
		return nil, false
	}
	return s.jobs.get(args.JobID)
}

// errorToolResult formats a typed error as an MCP tool-result so the
// LLM sees a structured error in the content block rather than a
// transport-level JSON-RPC error. Mirrors packErrorAsToolResult's
// shape for consistency with the sync path.
func errorToolResult(code, message string) map[string]any {
	body, _ := json.Marshal(map[string]string{"error": code, "message": message})
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(body)}},
		"isError": true,
	}
}

// mustJSON marshals a Go map to json.RawMessage and panics on error.
// Only used for the static async-tool schemas at startup, so a panic
// here would surface immediately on the first MCP connection.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("mcp: build async tool schema: " + err.Error())
	}
	return b
}
