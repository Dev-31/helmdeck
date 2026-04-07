// Package fake is an in-memory cdp.Client used by handler tests so the
// API layer can be exercised without a real Chromium.
package fake

import (
	"context"
	"errors"

	"github.com/tosin2013/helmdeck/internal/cdp"
)

// Client is a goroutine-safe stub cdp.Client. Test cases set the public
// fields to control return values, then assert against the captured ones.
type Client struct {
	NavigateURL    string
	ExtractCalls   []ExtractCall
	ScreenshotPNG  []byte
	ExecuteResult  any
	InteractCalls  []InteractCall
	NavigateErr    error
	ExtractText    string
	ExecuteErr     error
	InteractErr    error
	ScreenshotErr  error
	CloseCallCount int
}

// ExtractCall captures one Extract invocation.
type ExtractCall struct {
	Selector string
	Format   cdp.Format
}

// InteractCall captures one Interact invocation.
type InteractCall struct {
	Action   cdp.InteractAction
	Selector string
	Value    string
}

// Navigate implements cdp.Client.
func (c *Client) Navigate(_ context.Context, url string) error {
	c.NavigateURL = url
	return c.NavigateErr
}

// Extract implements cdp.Client.
func (c *Client) Extract(_ context.Context, selector string, format cdp.Format) (string, error) {
	c.ExtractCalls = append(c.ExtractCalls, ExtractCall{Selector: selector, Format: format})
	if c.ExtractText == "" {
		return "", errors.New("fake: ExtractText not set")
	}
	return c.ExtractText, nil
}

// Screenshot implements cdp.Client.
func (c *Client) Screenshot(_ context.Context, _ bool) ([]byte, error) {
	if c.ScreenshotErr != nil {
		return nil, c.ScreenshotErr
	}
	if c.ScreenshotPNG == nil {
		return []byte("\x89PNG\r\n\x1a\n-fake-"), nil
	}
	return c.ScreenshotPNG, nil
}

// Execute implements cdp.Client.
func (c *Client) Execute(_ context.Context, _ string) (any, error) {
	if c.ExecuteErr != nil {
		return nil, c.ExecuteErr
	}
	return c.ExecuteResult, nil
}

// Interact implements cdp.Client.
func (c *Client) Interact(_ context.Context, action cdp.InteractAction, selector, value string) error {
	c.InteractCalls = append(c.InteractCalls, InteractCall{Action: action, Selector: selector, Value: value})
	return c.InteractErr
}

// Close implements cdp.Client.
func (c *Client) Close() error {
	c.CloseCallCount++
	return nil
}

// compile-time interface check
var _ cdp.Client = (*Client)(nil)
