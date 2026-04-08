package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

// DocOCR (T403, ADR 019) wraps Tesseract as a one-call OCR pack. The
// pack accepts either a remote URL or a base64-encoded image, fetches
// the bytes in the control plane, pipes them into `tesseract - -`
// inside the session container, and returns the extracted text.
//
// Why fetch in helmdeck instead of inside the container: the session
// network is intentionally segmented from the public internet (T508
// will lock it down further). Pulling the URL in the control plane
// uses the existing AI gateway egress path and keeps OCR a strict
// "image bytes in, text out" function with no in-container HTTP.
//
// Tesseract supports `-l <lang>+<lang>+...` for multi-language
// recognition; the default is "eng" since that's the only language
// pack the sidecar Dockerfile installs by default.
//
// Input shape:
//
//	{
//	  "source_url": "https://example.com/scan.png",  // either this …
//	  "source_b64": "iVBORw0KGgo...",                 // … or this
//	  "language":   "eng"                             // optional, default eng
//	}
//
// Output shape:
//
//	{
//	  "text":     "Lorem ipsum...",
//	  "language": "eng",
//	  "bytes":    1234
//	}
func DocOCR() *packs.Pack {
	return &packs.Pack{
		Name:        "doc.ocr",
		Version:     "v1",
		Description: "Run Tesseract OCR over an image and return the extracted text.",
		NeedsSession: true,
		InputSchema: packs.BasicSchema{
			Properties: map[string]string{
				"source_url": "string",
				"source_b64": "string",
				"language":   "string",
			},
		},
		OutputSchema: packs.BasicSchema{
			Required: []string{"text", "language", "bytes"},
			Properties: map[string]string{
				"text":     "string",
				"language": "string",
				"bytes":    "number",
			},
		},
		Handler: docOCRHandler,
	}
}

type docOCRInput struct {
	SourceURL string `json:"source_url"`
	SourceB64 string `json:"source_b64"`
	Language  string `json:"language"`
}

// httpClient is a package-level client so tests can swap it for a
// mock. The default has an aggressive timeout because OCR sources
// are typically small images and a slow upstream shouldn't hold a
// session container hostage.
var ocrHTTPClient = &http.Client{Timeout: 30 * time.Second}

// maxOCRBytes guards against pulling a multi-GB image into RAM.
// 32 MiB matches the desktop screenshot cap and is comfortably above
// any realistic page-scan size.
const maxOCRBytes = 32 << 20

func docOCRHandler(ctx context.Context, ec *packs.ExecutionContext) (json.RawMessage, error) {
	var in docOCRInput
	if err := json.Unmarshal(ec.Input, &in); err != nil {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
	}
	if in.SourceURL == "" && in.SourceB64 == "" {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput,
			Message: "either source_url or source_b64 is required"}
	}
	if in.SourceURL != "" && in.SourceB64 != "" {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput,
			Message: "set either source_url or source_b64, not both"}
	}
	if ec.Exec == nil {
		return nil, &packs.PackError{Code: packs.CodeSessionUnavailable,
			Message: "engine has no session executor"}
	}
	lang := strings.TrimSpace(in.Language)
	if lang == "" {
		lang = "eng"
	}

	var img []byte
	if in.SourceB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(in.SourceB64)
		if err != nil {
			return nil, &packs.PackError{Code: packs.CodeInvalidInput,
				Message: "source_b64 is not valid base64", Cause: err}
		}
		img = decoded
	} else {
		fetched, err := fetchOCRSource(ctx, in.SourceURL)
		if err != nil {
			return nil, err
		}
		img = fetched
	}
	if len(img) == 0 {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: "source image is empty"}
	}
	if len(img) > maxOCRBytes {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput,
			Message: fmt.Sprintf("source image %d bytes exceeds %d byte cap", len(img), maxOCRBytes)}
	}

	// `tesseract - -` reads the image from stdin and writes the
	// extracted text to stdout. -l selects the language pack(s).
	// `--psm 3` is the default ("fully automatic page segmentation,
	// no OSD") and is set explicitly so future tesseract upgrades
	// don't change the default behavior under us.
	res, err := ec.Exec(ctx, session.ExecRequest{
		Cmd:   []string{"tesseract", "-l", lang, "--psm", "3", "-", "-"},
		Stdin: img,
	})
	if err != nil {
		return nil, fmt.Errorf("exec tesseract: %w", err)
	}
	if res.ExitCode != 0 {
		stderr := string(res.Stderr)
		if len(stderr) > 1024 {
			stderr = stderr[:1024] + "...(truncated)"
		}
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
			Message: fmt.Sprintf("tesseract exit %d: %s", res.ExitCode, stderr)}
	}

	text := strings.TrimRight(string(res.Stdout), "\n\r ")
	return json.Marshal(map[string]any{
		"text":     text,
		"language": lang,
		"bytes":    len(img),
	})
}

func fetchOCRSource(ctx context.Context, rawURL string) ([]byte, error) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput,
			Message: "source_url must be http or https"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &packs.PackError{Code: packs.CodeInvalidInput, Message: err.Error(), Cause: err}
	}
	req.Header.Set("User-Agent", "helmdeck-doc-ocr/1")
	resp, err := ocrHTTPClient.Do(req)
	if err != nil {
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
			Message: fmt.Sprintf("fetch %s: %v", rawURL, err), Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed,
			Message: fmt.Sprintf("fetch %s: HTTP %d", rawURL, resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOCRBytes+1))
	if err != nil {
		return nil, &packs.PackError{Code: packs.CodeHandlerFailed, Message: err.Error(), Cause: err}
	}
	return body, nil
}
