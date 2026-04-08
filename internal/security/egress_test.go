package security

import (
	"context"
	"errors"
	"net"
	"testing"
)

// stubResolver returns canned addresses keyed by host. The empty
// slice means "no records found"; an error means "lookup failed".
type stubResolver struct {
	results map[string][]net.IPAddr
	err     error
}

func (s stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.results[host], nil
}

func ip(s string) net.IPAddr { return net.IPAddr{IP: net.ParseIP(s)} }

func TestCheckIP_BlocksMetadataIP(t *testing.T) {
	g := New()
	if err := g.checkIP(net.ParseIP("169.254.169.254")); !errors.Is(err, ErrBlocked) {
		t.Errorf("expected metadata IP to be blocked, got %v", err)
	}
}

func TestCheckIP_BlocksRFC1918(t *testing.T) {
	g := New()
	cases := []string{"10.0.0.1", "172.16.5.5", "192.168.1.1"}
	for _, c := range cases {
		if err := g.checkIP(net.ParseIP(c)); !errors.Is(err, ErrBlocked) {
			t.Errorf("expected %s to be blocked", c)
		}
	}
}

func TestCheckIP_BlocksLoopback(t *testing.T) {
	g := New()
	for _, c := range []string{"127.0.0.1", "::1"} {
		if err := g.checkIP(net.ParseIP(c)); !errors.Is(err, ErrBlocked) {
			t.Errorf("expected %s to be blocked", c)
		}
	}
}

func TestCheckIP_BlocksIPv6LinkLocalAndULA(t *testing.T) {
	g := New()
	for _, c := range []string{"fe80::1", "fc00::1"} {
		if err := g.checkIP(net.ParseIP(c)); !errors.Is(err, ErrBlocked) {
			t.Errorf("expected %s to be blocked", c)
		}
	}
}

func TestCheckIP_AllowsPublic(t *testing.T) {
	g := New()
	for _, c := range []string{"8.8.8.8", "1.1.1.1", "140.82.114.4"} { // github.com
		if err := g.checkIP(net.ParseIP(c)); err != nil {
			t.Errorf("expected %s to pass, got %v", c, err)
		}
	}
}

func TestCheckIP_AllowlistOverridesBlock(t *testing.T) {
	g := New(WithAllowlist([]string{"10.20.30.0/24"}))
	if err := g.checkIP(net.ParseIP("10.20.30.5")); err != nil {
		t.Errorf("allowlisted IP should pass, got %v", err)
	}
	// Outside the allowlist but still RFC 1918 — must remain blocked.
	if err := g.checkIP(net.ParseIP("10.20.31.5")); !errors.Is(err, ErrBlocked) {
		t.Errorf("non-allowlisted RFC1918 should still be blocked")
	}
}

func TestCheckHost_LiteralIP(t *testing.T) {
	g := New(WithResolver(stubResolver{}))
	if err := g.CheckHost(context.Background(), "169.254.169.254"); !errors.Is(err, ErrBlocked) {
		t.Errorf("literal metadata IP should block without DNS")
	}
}

func TestCheckHost_StripsPort(t *testing.T) {
	g := New(WithResolver(stubResolver{}))
	if err := g.CheckHost(context.Background(), "127.0.0.1:8080"); !errors.Is(err, ErrBlocked) {
		t.Errorf("host:port should be parsed and blocked")
	}
}

func TestCheckHost_FQDN_AllPublic(t *testing.T) {
	g := New(WithResolver(stubResolver{
		results: map[string][]net.IPAddr{
			"github.com": {ip("140.82.114.4")},
		},
	}))
	if err := g.CheckHost(context.Background(), "github.com"); err != nil {
		t.Errorf("public FQDN should pass: %v", err)
	}
}

func TestCheckHost_FQDN_DNSRebindingAttack(t *testing.T) {
	// Classic SSRF: attacker controls DNS for evil.example, returns
	// one public IP and one metadata IP. The guard MUST refuse.
	g := New(WithResolver(stubResolver{
		results: map[string][]net.IPAddr{
			"evil.example": {ip("8.8.8.8"), ip("169.254.169.254")},
		},
	}))
	if err := g.CheckHost(context.Background(), "evil.example"); !errors.Is(err, ErrBlocked) {
		t.Errorf("multi-record DNS rebinding should fail closed, got %v", err)
	}
}

func TestCheckHost_NoRecords(t *testing.T) {
	g := New(WithResolver(stubResolver{results: map[string][]net.IPAddr{"x": {}}}))
	if err := g.CheckHost(context.Background(), "x"); !errors.Is(err, ErrBlocked) {
		t.Errorf("no-records lookup should fail closed")
	}
}

func TestCheckHost_EmptyHostBlocks(t *testing.T) {
	g := New()
	if err := g.CheckHost(context.Background(), ""); !errors.Is(err, ErrBlocked) {
		t.Errorf("empty host should block")
	}
}

func TestCheckURL(t *testing.T) {
	g := New(WithResolver(stubResolver{
		results: map[string][]net.IPAddr{"example.com": {ip("93.184.216.34")}},
	}))
	if err := g.CheckURL(context.Background(), "https://example.com/path"); err != nil {
		t.Errorf("public url should pass: %v", err)
	}
	if err := g.CheckURL(context.Background(), "http://169.254.169.254/latest/meta-data"); !errors.Is(err, ErrBlocked) {
		t.Errorf("imds url should block")
	}
	if err := g.CheckURL(context.Background(), "not a url"); err == nil {
		t.Errorf("malformed url should error")
	}
}

func TestAllowlistFromEnv(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"10.0.0.0/8", 1},
		{"10.0.0.0/8,192.168.1.0/24", 2},
		{" 10.0.0.0/8 , 192.168.1.0/24 ", 2}, // whitespace tolerated
		{",,10.0.0.0/8,,", 1},                 // empties dropped
	}
	for _, tc := range cases {
		got := AllowlistFromEnv(tc.in)
		if len(got) != tc.want {
			t.Errorf("AllowlistFromEnv(%q) = %v (len %d), want len %d", tc.in, got, len(got), tc.want)
		}
	}
}

func TestNew_AllowlistOption(t *testing.T) {
	g := New(WithAllowlist([]string{"172.16.0.0/16", "not-a-cidr"}))
	if len(g.allowlist) != 1 {
		t.Errorf("expected 1 valid allowlist entry, got %d", len(g.allowlist))
	}
	// Bad CIDR is dropped silently — failing-open on a single bad
	// operator config beats refusing to start the entire control plane.
	if err := g.checkIP(net.ParseIP("172.16.5.5")); err != nil {
		t.Errorf("172.16/16 allowlisted, should pass, got %v", err)
	}
}
