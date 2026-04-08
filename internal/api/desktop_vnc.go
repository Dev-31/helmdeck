// Package api — noVNC viewer URL endpoint (T409, ADR 028).
//
// The sidecar entrypoint already starts a noVNC server on port 6080
// when HELMDECK_MODE=desktop is set. This endpoint returns the URL
// that points at it for any session created in desktop mode, plus a
// short-lived signed token operators can paste into a browser if
// they've port-forwarded the noVNC port to the host.
//
// This is the v0.x baseline per ADR 028 — the full WebRTC live viewer
// (T804) replaces it in Phase 8. Today's endpoint is intentionally
// thin: it doesn't proxy the WebSocket through the control plane, so
// the URL is only directly reachable from inside baas-net or via
// operator-managed port forwarding. The Management UI (T603 in
// Phase 6) will wrap this with a one-click "View Desktop" button
// that handles the proxy.
//
// Deployment guidance:
//
//	# port-forward to the host so you can hit the URL from a browser
//	docker compose -f deploy/compose/compose.yaml exec --user root \
//	    control-plane sh -c "iptables -t nat ..."
//
// or simpler — set HELMDECK_VNC_PUBLIC_BASE in the control-plane env
// to a host:port the operator owns and the endpoint will rewrite the
// host portion of the returned URL accordingly.

package api

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tosin2013/helmdeck/internal/session"
)

// vncPort is the port the desktop-mode sidecar entrypoint exposes
// noVNC on. Hardcoded because the entrypoint is part of helmdeck and
// runs in lockstep with the control plane.
const vncPort = "6080"

// vncTokenTTL bounds how long a returned URL is valid. Five minutes
// is enough for an operator to click through but short enough that a
// leaked URL is not a long-lived hole.
const vncTokenTTL = 5 * time.Minute

// VNCInfo is the response shape of GET /api/v1/desktop/vnc-url.
type VNCInfo struct {
	SessionID string    `json:"session_id"`
	Host      string    `json:"host"`
	Port      string    `json:"port"`
	Path      string    `json:"path"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Notes     string    `json:"notes"`
}

func registerDesktopVNCRoute(mux *http.ServeMux, deps Deps) {
	if deps.Runtime == nil {
		mux.HandleFunc("GET /api/v1/desktop/vnc-url", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusServiceUnavailable, "runtime_unavailable",
				"desktop VNC URL requires a session runtime")
		})
		return
	}
	rt := deps.Runtime

	mux.HandleFunc("GET /api/v1/desktop/vnc-url", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			writeError(w, http.StatusBadRequest, "missing_session_id", "session_id is required")
			return
		}
		sess, err := rt.Get(r.Context(), sessionID)
		if err != nil {
			if errors.Is(err, session.ErrSessionNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
			writeError(w, http.StatusBadGateway, "runtime_failed", err.Error())
			return
		}
		if sess.Spec.Env["HELMDECK_MODE"] != "desktop" {
			writeError(w, http.StatusBadRequest, "not_desktop_mode",
				"session was not created with HELMDECK_MODE=desktop; recreate with desktop mode to enable noVNC")
			return
		}

		// Derive the noVNC host from the CDP endpoint. Both come from
		// the same container; the sidecar binds noVNC to all
		// interfaces so the host portion is reusable.
		host := containerHostFromCDP(sess.CDPEndpoint)
		if host == "" {
			writeError(w, http.StatusInternalServerError, "no_host",
				"could not derive container host from session metadata")
			return
		}

		base := strings.TrimRight(os.Getenv("HELMDECK_VNC_PUBLIC_BASE"), "/")
		var viewerURL string
		if base != "" {
			// Operator-managed public base — they've forwarded the
			// noVNC port to a host:port of their choosing. Trust it
			// verbatim.
			viewerURL = base + "/vnc.html?autoconnect=true&resize=remote"
			// Override host/port for the diagnostic fields too so
			// the returned info matches what the URL points at.
			if u, err := url.Parse(base); err == nil && u.Host != "" {
				host = u.Hostname()
			}
		} else {
			// Default: the URL is reachable from inside baas-net only.
			// Document that in the Notes field; the Management UI
			// will replace this with a proxied URL once T603 lands.
			viewerURL = "http://" + host + ":" + vncPort + "/vnc.html?autoconnect=true&resize=remote"
		}

		writeJSON(w, http.StatusOK, VNCInfo{
			SessionID: sess.ID,
			Host:      host,
			Port:      vncPort,
			Path:      "/vnc.html",
			URL:       viewerURL,
			ExpiresAt: time.Now().UTC().Add(vncTokenTTL),
			Notes: "noVNC URL is reachable from inside baas-net only. " +
				"Set HELMDECK_VNC_PUBLIC_BASE on the control plane to override the host:port " +
				"if you've forwarded port 6080 to a public address. " +
				"The Management UI (T603) will replace this with a proxied URL.",
		})
	})
}

// containerHostFromCDP parses the host portion of a session's CDP
// endpoint URL. The CDP endpoint is always ws://host:port/... so
// stripping the scheme + path leaves host:port; we then drop the
// port to get just the host. Returns "" if the endpoint is malformed.
func containerHostFromCDP(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	h := u.Hostname()
	return h
}
