import { PanelStub } from './_stub';

export function AuditPage() {
  return (
    <PanelStub
      title="Audit Log"
      description="Filter, search, and inspect every API call, pack invocation, and credential read."
      hint="Audit entries are written to the SQLite database today (table: audit_log). Query directly via the database file or via the upcoming GET /api/v1/audit endpoint."
    />
  );
}
