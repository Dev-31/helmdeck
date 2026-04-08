package docker

import (
	"strings"
	"testing"
)

// T509 sandbox spec tests. These exercise buildHostConfig directly
// so we don't need a real docker daemon — every assertion is on the
// HostConfig struct the runtime would hand to ContainerCreate.

func TestBuildHostConfig_DefaultsCapDropALL(t *testing.T) {
	r := &Runtime{pidsLimit: defaultPidsLimit}
	hc := r.buildHostConfig(1<<30, 1<<30, 1.0)
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", hc.CapDrop)
	}
	if len(hc.CapAdd) != 1 || hc.CapAdd[0] != "SYS_ADMIN" {
		t.Errorf("CapAdd = %v, want [SYS_ADMIN]", hc.CapAdd)
	}
}

func TestBuildHostConfig_DefaultsNoNewPrivileges(t *testing.T) {
	r := &Runtime{pidsLimit: defaultPidsLimit}
	hc := r.buildHostConfig(1<<30, 1<<30, 1.0)
	found := false
	for _, opt := range hc.SecurityOpt {
		if opt == "no-new-privileges:true" {
			found = true
		}
	}
	if !found {
		t.Errorf("SecurityOpt missing no-new-privileges:true: %v", hc.SecurityOpt)
	}
}

func TestBuildHostConfig_DefaultsPidsLimit(t *testing.T) {
	r := &Runtime{pidsLimit: defaultPidsLimit}
	hc := r.buildHostConfig(1<<30, 1<<30, 1.0)
	if hc.PidsLimit == nil || *hc.PidsLimit != defaultPidsLimit {
		t.Errorf("PidsLimit = %v, want %d", hc.PidsLimit, defaultPidsLimit)
	}
}

func TestBuildHostConfig_CustomPidsLimit(t *testing.T) {
	r := &Runtime{pidsLimit: 256}
	hc := r.buildHostConfig(1<<30, 1<<30, 1.0)
	if hc.PidsLimit == nil || *hc.PidsLimit != 256 {
		t.Errorf("PidsLimit = %v, want 256", hc.PidsLimit)
	}
}

func TestBuildHostConfig_SeccompProfile(t *testing.T) {
	r := &Runtime{pidsLimit: defaultPidsLimit, seccompProfile: "/etc/helmdeck/chrome.json"}
	hc := r.buildHostConfig(1<<30, 1<<30, 1.0)
	found := false
	for _, opt := range hc.SecurityOpt {
		if strings.HasPrefix(opt, "seccomp=") && strings.Contains(opt, "chrome.json") {
			found = true
		}
	}
	if !found {
		t.Errorf("SecurityOpt missing seccomp=<profile>: %v", hc.SecurityOpt)
	}
}

func TestBuildHostConfig_NoSeccompFallbackToDefault(t *testing.T) {
	// Empty seccompProfile MUST mean "use docker's default profile"
	// — i.e. we omit the seccomp= entry entirely. Docker applies
	// its built-in profile when no override is set.
	r := &Runtime{pidsLimit: defaultPidsLimit}
	hc := r.buildHostConfig(1<<30, 1<<30, 1.0)
	for _, opt := range hc.SecurityOpt {
		if strings.HasPrefix(opt, "seccomp=") {
			t.Errorf("empty seccompProfile should NOT add a seccomp= entry, got %q", opt)
		}
	}
}

func TestBuildHostConfig_NetworkAttachedWhenSet(t *testing.T) {
	r := &Runtime{pidsLimit: defaultPidsLimit, network: "baas-net"}
	hc := r.buildHostConfig(1<<30, 1<<30, 1.0)
	if string(hc.NetworkMode) != "baas-net" {
		t.Errorf("NetworkMode = %q, want baas-net", hc.NetworkMode)
	}
}

func TestBuildHostConfig_ResourceLimitsApplied(t *testing.T) {
	r := &Runtime{pidsLimit: defaultPidsLimit}
	hc := r.buildHostConfig(2<<30, 1<<30, 1.5)
	if hc.Memory != 2<<30 {
		t.Errorf("Memory = %d", hc.Memory)
	}
	if hc.NanoCPUs != int64(1.5*1e9) {
		t.Errorf("NanoCPUs = %d", hc.NanoCPUs)
	}
	if hc.ShmSize != 1<<30 {
		t.Errorf("ShmSize = %d", hc.ShmSize)
	}
}

func TestNew_DefaultsPidsLimit(t *testing.T) {
	r := &Runtime{}
	// Direct field access — calling New() requires a docker daemon
	// because of NewClientWithOpts. Verify that buildHostConfig
	// receives defaultPidsLimit when constructed via the option.
	WithPidsLimit(defaultPidsLimit)(r)
	if r.pidsLimit != defaultPidsLimit {
		t.Errorf("WithPidsLimit didn't apply: %d", r.pidsLimit)
	}
}

func TestWithSeccompProfile(t *testing.T) {
	r := &Runtime{}
	WithSeccompProfile("/path/to/profile.json")(r)
	if r.seccompProfile != "/path/to/profile.json" {
		t.Errorf("WithSeccompProfile didn't apply: %q", r.seccompProfile)
	}
}
