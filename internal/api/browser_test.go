package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/cdp"
	cdpfake "github.com/tosin2013/helmdeck/internal/cdp/fake"
)

type stubCDPFactory struct {
	c       cdp.Client
	evicted []string
}

func (s *stubCDPFactory) Get(_ context.Context, _ string) (cdp.Client, error) {
	return s.c, nil
}
func (s *stubCDPFactory) Evict(id string) { s.evicted = append(s.evicted, id) }

func newBrowserRouter(t *testing.T) (http.Handler, *cdpfake.Client) {
	t.Helper()
	fc := &cdpfake.Client{ExtractText: "Hello World", ExecuteResult: 42}
	h := NewRouter(Deps{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:    "test",
		CDPFactory: &stubCDPFactory{c: fc},
	})
	return h, fc
}

func TestBrowserNavigate(t *testing.T) {
	h, fc := newBrowserRouter(t)
	body := bytes.NewBufferString(`{"session_id":"s1","url":"https://example.com"}`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/browser/navigate", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if fc.NavigateURL != "https://example.com" {
		t.Fatalf("Navigate URL = %q", fc.NavigateURL)
	}
}

func TestBrowserExtract(t *testing.T) {
	h, fc := newBrowserRouter(t)
	body := bytes.NewBufferString(`{"session_id":"s1","selector":".price","format":"text"}`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/browser/extract", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["content"] != "Hello World" {
		t.Fatalf("content = %v", resp["content"])
	}
	if len(fc.ExtractCalls) != 1 || fc.ExtractCalls[0].Selector != ".price" {
		t.Fatalf("ExtractCalls = %+v", fc.ExtractCalls)
	}
}

func TestBrowserScreenshot(t *testing.T) {
	h, _ := newBrowserRouter(t)
	body := bytes.NewBufferString(`{"session_id":"s1","full_page":true}`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/browser/screenshot", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if rr.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("content-type = %q", rr.Header().Get("Content-Type"))
	}
	if !bytes.HasPrefix(rr.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("body does not start with PNG magic: %q", rr.Body.String())
	}
}

func TestBrowserExecute(t *testing.T) {
	h, _ := newBrowserRouter(t)
	body := bytes.NewBufferString(`{"session_id":"s1","script":"document.title"}`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/browser/execute", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"result":42`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestBrowserInteractClick(t *testing.T) {
	h, fc := newBrowserRouter(t)
	body := bytes.NewBufferString(`{"session_id":"s1","action":"click","selector":"#submit"}`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/browser/interact", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(fc.InteractCalls) != 1 || fc.InteractCalls[0].Action != cdp.ActionClick {
		t.Fatalf("InteractCalls = %+v", fc.InteractCalls)
	}
}

func TestBrowserMissingURLBadRequest(t *testing.T) {
	h, _ := newBrowserRouter(t)
	rr := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"session_id":"s1"}`)
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/browser/navigate", body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestBrowserNoFactoryReturns503(t *testing.T) {
	h := NewRouter(Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "test",
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/browser/navigate", bytes.NewBufferString(`{}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
