import { PanelStub } from './_stub';

export function SecurityPage() {
  return (
    <PanelStub
      title="Security Policies"
      description="Egress allowlist, sandbox baseline, and access control rules."
      hint="Today's policies are configured via the HELMDECK_EGRESS_ALLOWLIST, HELMDECK_PIDS_LIMIT, and HELMDECK_SECCOMP_PROFILE env vars on the control plane. See docs/SECURITY-HARDENING.md for the full operator runbook."
    />
  );
}
