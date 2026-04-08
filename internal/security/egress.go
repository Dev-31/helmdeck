// Package security provides shared safety primitives used by pack
// handlers, the credential vault injector, and the future
// placeholder-token egress gateway (T504).
//
// EgressGuard (T508) is the application-layer SSRF defense: a single
// validator that pack handlers and resolvers consult before they let
// a URL or hostname touch a real network call. It blocks cloud
// metadata IPs (169.254.169.254 and the wider link-local range), all
// RFC 1918 private ranges, loopback, and IPv6 equivalents — the same
// shape kubernetes NetworkPolicy egress rules will enforce in
// Phase 7 (T706), but expressed in Go so it travels with the binary
// and protects single-node Compose deployments without an iptables
// runbook.
//
// Operators with internal CI servers or self-hosted git that need to
// be reachable from session containers can whitelist them via the
// HELMDECK_EGRESS_ALLOWLIST env var (comma-separated CIDRs).
package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// ErrBlocked is returned when a host or address resolves to a
// blocked range. Pack handlers should propagate this as a typed
// invalid_input error so the LLM gets a clear "not allowed" signal
// instead of a network failure later in the pipeline.
var ErrBlocked = errors.New("security: destination is in a blocked address range")

// EgressGuard decides whether a host/URL is allowed to leave the
// helmdeck control plane or session containers. Goroutine-safe; one
// guard per process is the expected pattern.
type EgressGuard struct {
	blocked   []*net.IPNet
	allowlist []*net.IPNet
	resolver  Resolver
	timeout   time.Duration
}

// Resolver is the DNS surface the guard depends on. The default uses
// net.DefaultResolver; tests inject a stub so they can return canned
// addresses without touching real DNS.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Option configures an EgressGuard at construction time.
type Option func(*EgressGuard)

// WithResolver overrides the DNS resolver. Default: net.DefaultResolver.
func WithResolver(r Resolver) Option { return func(g *EgressGuard) { g.resolver = r } }

// WithTimeout caps DNS lookup time. Default: 5 seconds. Zero means
// "use default"; negative means "no timeout" (not recommended).
func WithTimeout(d time.Duration) Option {
	return func(g *EgressGuard) {
		if d > 0 {
			g.timeout = d
		}
	}
}

// WithAllowlist appends CIDRs to the allowlist. Allowlisted ranges
// override the default block list — an operator who allowlists
// 10.20.0.0/16 will see the guard let 10.20.0.5 through even though
// it falls within the RFC 1918 default block.
func WithAllowlist(cidrs []string) Option {
	return func(g *EgressGuard) {
		for _, cidr := range cidrs {
			cidr = strings.TrimSpace(cidr)
			if cidr == "" {
				continue
			}
			if _, n, err := net.ParseCIDR(cidr); err == nil {
				g.allowlist = append(g.allowlist, n)
			}
		}
	}
}

// New constructs an EgressGuard with the default block list applied.
// The default block list covers everything operators almost always
// want blocked from a session container:
//
//   - 169.254.169.254/32 — AWS/GCP/Azure instance metadata service
//   - 169.254.0.0/16     — full link-local range (covers AWS IMDSv2 etc.)
//   - 127.0.0.0/8        — loopback
//   - 10.0.0.0/8         — RFC 1918 private
//   - 172.16.0.0/12      — RFC 1918 private
//   - 192.168.0.0/16     — RFC 1918 private
//   - 100.64.0.0/10      — carrier-grade NAT (Tailscale, ISPs)
//   - 0.0.0.0/8          — "this network", commonly abused for SSRF
//   - 224.0.0.0/4        — multicast
//   - ::1/128            — IPv6 loopback
//   - fc00::/7           — IPv6 unique local
//   - fe80::/10          — IPv6 link-local
//   - ff00::/8           — IPv6 multicast
//
// Operators who legitimately need to reach RFC 1918 hosts (an
// internal git server, a self-hosted CI dashboard) opt back in via
// WithAllowlist or HELMDECK_EGRESS_ALLOWLIST.
func New(opts ...Option) *EgressGuard {
	g := &EgressGuard{
		blocked:  defaultBlockedRanges(),
		resolver: net.DefaultResolver,
		timeout:  5 * time.Second,
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// CheckHost validates a hostname (FQDN or literal IP). Returns
// ErrBlocked if the host resolves to any blocked address that isn't
// covered by the allowlist. An empty host is treated as blocked
// because allowing "" tends to be a sign of upstream parser bugs.
func (g *EgressGuard) CheckHost(ctx context.Context, host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrBlocked)
	}
	// Strip a trailing :port if present (the caller may pass host:port).
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// Literal IP fast path: no DNS round trip.
	if ip := net.ParseIP(host); ip != nil {
		return g.checkIP(ip)
	}
	// FQDN — resolve and check every returned address. We require
	// EVERY address to be allowed; a single blocked address fails
	// the check (otherwise an attacker could control DNS to return
	// one allowed IP and one metadata IP and have the request go to
	// whichever the kernel picks).
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("security: dns lookup %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %q resolved to no addresses", ErrBlocked, host)
	}
	for _, a := range addrs {
		if err := g.checkIP(a.IP); err != nil {
			return err
		}
	}
	return nil
}

// CheckURL validates the host portion of a URL. Convenience wrapper
// for the common pack-handler call site.
func (g *EgressGuard) CheckURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("security: parse url %q: %w", rawURL, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: url has no host: %s", ErrBlocked, rawURL)
	}
	return g.CheckHost(ctx, u.Hostname())
}

// checkIP returns nil if ip is allowed, ErrBlocked otherwise.
// Allowlist always wins over the block list.
func (g *EgressGuard) checkIP(ip net.IP) error {
	for _, allow := range g.allowlist {
		if allow.Contains(ip) {
			return nil
		}
	}
	for _, block := range g.blocked {
		if block.Contains(ip) {
			return fmt.Errorf("%w: %s is in %s", ErrBlocked, ip, block)
		}
	}
	return nil
}

// defaultBlockedRanges returns the canonical helmdeck block list.
// Centralised so tests can assert on the membership without
// re-parsing the constants.
func defaultBlockedRanges() []*net.IPNet {
	cidrs := []string{
		// IPv4
		"169.254.169.254/32", // cloud metadata IMDS (most specific first for clarity)
		"169.254.0.0/16",     // link-local — covers all IMDS variants
		"127.0.0.0/8",        // loopback
		"10.0.0.0/8",         // RFC 1918
		"172.16.0.0/12",      // RFC 1918
		"192.168.0.0/16",     // RFC 1918
		"100.64.0.0/10",      // carrier-grade NAT
		"0.0.0.0/8",          // "this network"
		"224.0.0.0/4",        // multicast
		// IPv6
		"::1/128",  // loopback
		"fc00::/7", // unique local
		"fe80::/10", // link-local
		"ff00::/8", // multicast
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			// Compile-time bug — every CIDR above is hardcoded and
			// must parse. Skip silently rather than panic so a
			// future typo doesn't take down a whole control plane.
			continue
		}
		out = append(out, n)
	}
	return out
}

// AllowlistFromEnv parses HELMDECK_EGRESS_ALLOWLIST and returns the
// CIDR list as a []string the constructor can consume. Empty values
// are dropped; whitespace is trimmed. Centralised so the control
// plane and any test harness use the same parser.
func AllowlistFromEnv(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
