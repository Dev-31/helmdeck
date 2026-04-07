// e2e tests for the cdp package. Skipped automatically when neither a
// HELMDECK_CDP_ENDPOINT env var nor a local helmdeck-sidecar:dev image
// is available, so `make ci` stays green on machines without Docker
// or the sidecar built.
package cdp_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tosin2013/helmdeck/internal/cdp"
)

func dockerImageExists(t *testing.T, ref string) bool {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", ref).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), `"Id"`)
}

// startSidecar runs helmdeck-sidecar:dev locally with random host port,
// returns the http://host:port endpoint, and a cleanup func.
func startSidecar(t *testing.T) (string, func()) {
	t.Helper()
	if !dockerImageExists(t, "helmdeck-sidecar:dev") {
		t.Skip("helmdeck-sidecar:dev image not present; run `make sidecar-build` first")
	}
	id, err := exec.Command("docker", "run", "-d", "--rm",
		"--shm-size=1g",
		"-p", "0:9222", // random host port
		"--security-opt=no-new-privileges:true",
		"--cap-drop=ALL", "--cap-add=SYS_ADMIN",
		"helmdeck-sidecar:dev",
	).Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	cid := strings.TrimSpace(string(id))
	cleanup := func() {
		_ = exec.Command("docker", "stop", cid).Run()
	}

	// Resolve the random host port docker assigned.
	portOut, err := exec.Command("docker", "port", cid, "9222/tcp").Output()
	if err != nil {
		cleanup()
		t.Fatalf("docker port: %v", err)
	}
	// "0.0.0.0:43217\n[::]:43217\n" — take the first line.
	hostPort := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
	endpoint := "http://" + hostPort

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cdp.WaitReady(ctx, endpoint, 200*time.Millisecond); err != nil {
		_ = exec.Command("docker", "logs", cid).Run()
		cleanup()
		t.Fatalf("WaitReady: %v", err)
	}
	return endpoint, cleanup
}

func TestRealChromiumNavigateScreenshot(t *testing.T) {
	if os.Getenv("CI") != "" && os.Getenv("HELMDECK_E2E") == "" {
		t.Skip("e2e disabled in CI without HELMDECK_E2E=1")
	}
	endpoint, cleanup := startSidecar(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := cdp.New(ctx, endpoint)
	if err != nil {
		t.Fatalf("cdp.New: %v", err)
	}
	defer c.Close()

	if err := c.Navigate(ctx, "data:text/html,<html><body><h1>helmdeck-e2e</h1></body></html>"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	text, err := c.Extract(ctx, "h1", cdp.FormatText)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(text, "helmdeck-e2e") {
		t.Fatalf("Extract text = %q, want substring helmdeck-e2e", text)
	}

	png, err := c.Screenshot(ctx, false)
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if len(png) < 100 {
		t.Fatalf("screenshot too small: %d bytes", len(png))
	}
	if string(png[:4]) != "\x89PNG" {
		t.Fatalf("not a PNG: % x", png[:8])
	}

	result, err := c.Execute(ctx, "1+1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if v, ok := result.(float64); !ok || v != 2 {
		t.Fatalf("Execute result = %v (%T), want 2", result, result)
	}
}
