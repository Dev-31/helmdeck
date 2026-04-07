package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/tosin2013/helmdeck/internal/cdp"
	"github.com/tosin2013/helmdeck/internal/session"
)

// CDPClientFactory owns the lifecycle of cdp.Client instances for browser
// sessions. Each session gets exactly one chromedp client that survives
// across HTTP requests — chromedp's connection model expects a long-lived
// browser context, and dialing per request leaks Chromium tabs and trips
// "context canceled" on every call after the first.
//
// Get returns a cached client (creating one on first use) and the caller
// must NOT call Close on it — the factory owns it. Evict closes and
// removes the cached client and is called from the sessions DELETE
// handler before the underlying container is terminated.
type CDPClientFactory interface {
	Get(ctx context.Context, sessionID string) (cdp.Client, error)
	Evict(sessionID string)
}

// DefaultCDPClientFactory returns a CDPClientFactory that resolves the
// session via rt and dials chromedp at session.CDPEndpoint, caching the
// resulting client by session id.
func DefaultCDPClientFactory(rt session.Runtime) CDPClientFactory {
	return &defaultCDPFactory{rt: rt}
}

type defaultCDPFactory struct {
	rt    session.Runtime
	mu    sync.Mutex
	cache map[string]cdp.Client
}

func (f *defaultCDPFactory) Get(ctx context.Context, id string) (cdp.Client, error) {
	f.mu.Lock()
	if f.cache == nil {
		f.cache = make(map[string]cdp.Client)
	}
	if c, ok := f.cache[id]; ok {
		f.mu.Unlock()
		return c, nil
	}
	f.mu.Unlock()

	s, err := f.rt.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.CDPEndpoint == "" {
		return nil, errors.New("session has no CDP endpoint")
	}

	// Use a background context as the chromedp parent so the cached client
	// outlives the HTTP request that created it. Eviction explicitly tears
	// it down via Close.
	c, err := cdp.New(context.Background(), s.CDPEndpoint)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Race: another goroutine may have populated the cache in the meantime.
	if existing, ok := f.cache[id]; ok {
		_ = c.Close()
		return existing, nil
	}
	f.cache[id] = c
	return c, nil
}

func (f *defaultCDPFactory) Evict(id string) {
	f.mu.Lock()
	c, ok := f.cache[id]
	delete(f.cache, id)
	f.mu.Unlock()
	if ok {
		_ = c.Close()
	}
}

type navigateRequest struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
}

type extractRequest struct {
	SessionID string `json:"session_id"`
	Selector  string `json:"selector"`
	Format    string `json:"format,omitempty"`
}

type screenshotRequest struct {
	SessionID string `json:"session_id"`
	FullPage  bool   `json:"full_page"`
}

type executeRequest struct {
	SessionID string `json:"session_id"`
	Script    string `json:"script"`
}

type interactRequest struct {
	SessionID string `json:"session_id"`
	Action    string `json:"action"`
	Selector  string `json:"selector"`
	Value     string `json:"value,omitempty"`
}

func registerBrowserRoutes(mux *http.ServeMux, deps Deps) {
	if deps.CDPFactory == nil {
		mux.HandleFunc("/api/v1/browser/", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "cdp_unavailable", "browser CDP not configured")
		})
		return
	}
	f := deps.CDPFactory

	withClient := func(w http.ResponseWriter, r *http.Request, sessionID string, fn func(cdp.Client) error) {
		if sessionID == "" {
			writeError(w, http.StatusBadRequest, "missing_session_id", "session_id is required")
			return
		}
		c, err := f.Get(r.Context(), sessionID)
		if err != nil {
			if errors.Is(err, session.ErrSessionNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			writeError(w, http.StatusBadGateway, "cdp_dial_failed", err.Error())
			return
		}
		// Factory owns lifecycle — do NOT call c.Close().
		if err := fn(c); err != nil {
			writeError(w, http.StatusInternalServerError, "cdp_call_failed", err.Error())
		}
	}

	mux.HandleFunc("POST /api/v1/browser/navigate", func(w http.ResponseWriter, r *http.Request) {
		var req navigateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if req.URL == "" {
			writeError(w, http.StatusBadRequest, "missing_url", "url is required")
			return
		}
		withClient(w, r, req.SessionID, func(c cdp.Client) error {
			if err := c.Navigate(r.Context(), req.URL); err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": req.URL})
			return nil
		})
	})

	mux.HandleFunc("POST /api/v1/browser/extract", func(w http.ResponseWriter, r *http.Request) {
		var req extractRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		format := cdp.FormatText
		if req.Format == "html" {
			format = cdp.FormatHTML
		}
		withClient(w, r, req.SessionID, func(c cdp.Client) error {
			out, err := c.Extract(r.Context(), req.Selector, format)
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, map[string]any{"content": out, "format": string(format)})
			return nil
		})
	})

	mux.HandleFunc("POST /api/v1/browser/screenshot", func(w http.ResponseWriter, r *http.Request) {
		var req screenshotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		withClient(w, r, req.SessionID, func(c cdp.Client) error {
			png, err := c.Screenshot(r.Context(), req.FullPage)
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(png)
			return nil
		})
	})

	mux.HandleFunc("POST /api/v1/browser/execute", func(w http.ResponseWriter, r *http.Request) {
		var req executeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if req.Script == "" {
			writeError(w, http.StatusBadRequest, "missing_script", "script is required")
			return
		}
		withClient(w, r, req.SessionID, func(c cdp.Client) error {
			result, err := c.Execute(r.Context(), req.Script)
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, map[string]any{"result": result})
			return nil
		})
	})

	mux.HandleFunc("POST /api/v1/browser/interact", func(w http.ResponseWriter, r *http.Request) {
		var req interactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		withClient(w, r, req.SessionID, func(c cdp.Client) error {
			if err := c.Interact(r.Context(), cdp.InteractAction(req.Action), req.Selector, req.Value); err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return nil
		})
	})
}
