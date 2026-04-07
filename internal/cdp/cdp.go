// Package cdp wraps chromedp with the operations the helmdeck control
// plane and Capability Packs actually need: navigate, extract, screenshot,
// execute, and interact. The interface is defined here so the API layer
// can be tested without a real Chromium.
//
// See ADR 002 (chromedp choice) and PRD §6.2 / §7.1.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// Format selects how Extract returns matched DOM content.
type Format string

const (
	FormatText Format = "text"
	FormatHTML Format = "html"
)

// InteractAction is the closed set of synthetic browser interactions
// supported by the Interact endpoint. New actions land here so the API
// vocabulary stays in lockstep with the implementation.
type InteractAction string

const (
	ActionClick InteractAction = "click"
	ActionType  InteractAction = "type"
	ActionFocus InteractAction = "focus"
)

// Client is the implementation-agnostic CDP surface used by REST handlers,
// pack handlers, and tests.
type Client interface {
	Navigate(ctx context.Context, url string) error
	Extract(ctx context.Context, selector string, format Format) (string, error)
	Screenshot(ctx context.Context, fullPage bool) ([]byte, error)
	Execute(ctx context.Context, script string) (any, error)
	Interact(ctx context.Context, action InteractAction, selector, value string) error
	Close() error
}

// WaitReady polls the CDP /json/version endpoint until it returns 200 OK
// or ctx is canceled. Used after a session container starts to know when
// Chromium is actually serving CDP.
func WaitReady(ctx context.Context, endpoint string, every time.Duration) error {
	if every <= 0 {
		every = 200 * time.Millisecond
	}
	url := strings.TrimSuffix(endpoint, "/") + "/json/version"
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cdp: wait ready: %w", ctx.Err())
		case <-t.C:
		}
	}
}

// chromedpClient is the production chromedp-backed Client. It owns a
// chromedp.RemoteAllocator and a chromedp.Context.
type chromedpClient struct {
	allocCancel context.CancelFunc
	ctxCancel   context.CancelFunc
	browserCtx  context.Context
}

// New constructs a Client connected to a Chromium instance reachable via
// the given http(s) endpoint (e.g. http://10.0.0.5:9222). Caller must
// call Close to release resources.
func New(parent context.Context, endpoint string) (Client, error) {
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(parent, normalizeEndpoint(endpoint))
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	// Trigger an immediate browser handshake so a dead endpoint surfaces
	// here rather than on the first real command.
	if err := chromedp.Run(ctx); err != nil {
		cancelCtx()
		cancelAlloc()
		return nil, fmt.Errorf("cdp: new: %w", err)
	}
	return &chromedpClient{
		allocCancel: cancelAlloc,
		ctxCancel:   cancelCtx,
		browserCtx:  ctx,
	}, nil
}

// normalizeEndpoint accepts http://, https://, ws://, wss:// or bare host:port
// and returns the http:// form chromedp expects.
func normalizeEndpoint(in string) string {
	switch {
	case strings.HasPrefix(in, "http://"), strings.HasPrefix(in, "https://"):
		return in
	case strings.HasPrefix(in, "ws://"):
		return "http://" + strings.TrimPrefix(in, "ws://")
	case strings.HasPrefix(in, "wss://"):
		return "https://" + strings.TrimPrefix(in, "wss://")
	default:
		return "http://" + in
	}
}

// Navigate implements Client.
func (c *chromedpClient) Navigate(ctx context.Context, url string) error {
	return chromedp.Run(c.browserCtx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
	)
}

// Extract implements Client.
func (c *chromedpClient) Extract(ctx context.Context, selector string, format Format) (string, error) {
	if selector == "" {
		selector = "html"
	}
	var out string
	var task chromedp.Action
	switch format {
	case FormatHTML:
		task = chromedp.OuterHTML(selector, &out, chromedp.ByQuery)
	default:
		task = chromedp.Text(selector, &out, chromedp.ByQueryAll, chromedp.NodeVisible)
	}
	if err := chromedp.Run(c.browserCtx, task); err != nil {
		return "", err
	}
	return out, nil
}

// Screenshot implements Client.
func (c *chromedpClient) Screenshot(ctx context.Context, fullPage bool) ([]byte, error) {
	var buf []byte
	var task chromedp.Action
	if fullPage {
		task = chromedp.FullScreenshot(&buf, 90)
	} else {
		task = chromedp.CaptureScreenshot(&buf)
	}
	if err := chromedp.Run(c.browserCtx, task); err != nil {
		return nil, err
	}
	return buf, nil
}

// Execute implements Client. It runs a JS expression and returns the
// JSON-decoded result.
func (c *chromedpClient) Execute(ctx context.Context, script string) (any, error) {
	var raw json.RawMessage
	if err := chromedp.Run(c.browserCtx, chromedp.Evaluate(script, &raw)); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), nil
	}
	return out, nil
}

// Interact implements Client.
func (c *chromedpClient) Interact(ctx context.Context, action InteractAction, selector, value string) error {
	if selector == "" {
		return errors.New("cdp: interact: selector is required")
	}
	switch action {
	case ActionClick:
		return chromedp.Run(c.browserCtx, chromedp.Click(selector, chromedp.ByQuery))
	case ActionType:
		return chromedp.Run(c.browserCtx,
			chromedp.Focus(selector, chromedp.ByQuery),
			chromedp.SendKeys(selector, value, chromedp.ByQuery),
		)
	case ActionFocus:
		return chromedp.Run(c.browserCtx, chromedp.Focus(selector, chromedp.ByQuery))
	}
	return fmt.Errorf("cdp: interact: unknown action %q", action)
}

// Close implements Client.
func (c *chromedpClient) Close() error {
	c.ctxCancel()
	c.allocCancel()
	return nil
}

// unused-import guard for cdproto packages while the implementation
// surface is small; runtime + dom are kept so future hooks (e.g.
// runtime.Evaluate with awaitPromise) don't trigger an import churn.
var (
	_ = runtime.Evaluate
	_ = dom.Enable
)
