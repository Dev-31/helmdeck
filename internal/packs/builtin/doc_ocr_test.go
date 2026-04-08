package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tosin2013/helmdeck/internal/packs"
	"github.com/tosin2013/helmdeck/internal/session"
)

func newOCREngine(t *testing.T, ex session.Executor) *packs.Engine {
	t.Helper()
	return packs.New(
		packs.WithRuntime(fakeRuntime{}),
		packs.WithSessionExecutor(ex),
	)
}

func TestDocOCRBase64Source(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("Lorem ipsum dolor\n")},
	}}
	eng := newOCREngine(t, ex)
	img := []byte("fakepngbytes")
	body := `{"source_b64":"` + base64.StdEncoding.EncodeToString(img) + `","language":"eng"}`

	res, err := eng.Execute(context.Background(), DocOCR(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(ex.calls))
	}
	cmd := ex.calls[0].Cmd
	if cmd[0] != "tesseract" || cmd[1] != "-l" || cmd[2] != "eng" {
		t.Errorf("bad argv: %v", cmd)
	}
	if string(ex.calls[0].Stdin) != string(img) {
		t.Errorf("stdin not propagated: got %d bytes", len(ex.calls[0].Stdin))
	}
	var out struct {
		Text     string `json:"text"`
		Language string `json:"language"`
		Bytes    int    `json:"bytes"`
	}
	_ = json.Unmarshal(res.Output, &out)
	if out.Text != "Lorem ipsum dolor" || out.Language != "eng" || out.Bytes != len(img) {
		t.Errorf("output = %+v", out)
	}
}

func TestDocOCRHTTPSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fetched-image-bytes"))
	}))
	defer srv.Close()

	ex := &recordingExecutor{replies: []session.ExecResult{
		{Stdout: []byte("hello world")},
	}}
	eng := newOCREngine(t, ex)
	body := `{"source_url":"` + srv.URL + `/scan.png"}`

	res, err := eng.Execute(context.Background(), DocOCR(), json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(ex.calls[0].Stdin) != "fetched-image-bytes" {
		t.Errorf("http body not piped to tesseract: %q", ex.calls[0].Stdin)
	}
	if !strings.Contains(string(res.Output), `"text":"hello world"`) {
		t.Errorf("text not in output: %s", res.Output)
	}
}

func TestDocOCRRequiresSource(t *testing.T) {
	eng := newOCREngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), DocOCR(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error when neither source_url nor source_b64 is set")
	}
}

func TestDocOCRRejectsBothSources(t *testing.T) {
	eng := newOCREngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), DocOCR(),
		json.RawMessage(`{"source_url":"https://x","source_b64":"YQ=="}`))
	if err == nil {
		t.Fatal("expected error when both source_url and source_b64 are set")
	}
}

func TestDocOCRRejectsNonHTTPURL(t *testing.T) {
	eng := newOCREngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), DocOCR(),
		json.RawMessage(`{"source_url":"file:///etc/passwd"}`))
	if err == nil {
		t.Fatal("expected error for non-http source_url")
	}
}

func TestDocOCRBase64Invalid(t *testing.T) {
	eng := newOCREngine(t, &recordingExecutor{})
	_, err := eng.Execute(context.Background(), DocOCR(),
		json.RawMessage(`{"source_b64":"not!!base64"}`))
	if err == nil {
		t.Fatal("expected error for malformed base64")
	}
}

func TestDocOCRTesseractFailure(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{
		{ExitCode: 1, Stderr: []byte("read_image_from_pipe: read")},
	}}
	eng := newOCREngine(t, ex)
	body := `{"source_b64":"` + base64.StdEncoding.EncodeToString([]byte("img")) + `"}`
	_, err := eng.Execute(context.Background(), DocOCR(), json.RawMessage(body))
	if err == nil {
		t.Fatal("expected tesseract failure to surface")
	}
	if !strings.Contains(err.Error(), "tesseract exit 1") {
		t.Errorf("error should mention tesseract exit: %v", err)
	}
}

func TestDocOCRDefaultsToEng(t *testing.T) {
	ex := &recordingExecutor{replies: []session.ExecResult{{Stdout: []byte("ok")}}}
	eng := newOCREngine(t, ex)
	body := `{"source_b64":"` + base64.StdEncoding.EncodeToString([]byte("x")) + `"}`
	if _, err := eng.Execute(context.Background(), DocOCR(), json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	if ex.calls[0].Cmd[2] != "eng" {
		t.Errorf("default language should be eng, got %s", ex.calls[0].Cmd[2])
	}
}
