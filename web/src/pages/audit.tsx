import { useState } from 'react';
import { FileSearch, RefreshCw, ShieldAlert } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { useApi } from '@/lib/queries';
import { formatRelative } from '@/lib/format';

interface AuditEntry {
  id: number;
  timestamp: string;
  severity: string;
  event_type: string;
  actor_subject?: string;
  actor_client?: string;
  session_id?: string;
  method?: string;
  path?: string;
  status_code: number;
  payload?: Record<string, unknown>;
}

interface AuditResponse {
  entries: AuditEntry[];
  count: number;
}

// Closed set of event types the backend writes today (see
// internal/audit/audit.go EventType constants). Keeping this in
// sync with the Go side is a documentation discipline; an out-of-set
// value still renders fine — the filter just won't match.
const EVENT_TYPES = [
  'api_request',
  'session_create',
  'session_terminate',
  'pack_call',
  'llm_call',
  'mcp_call',
  'vault_read',
  'key_rotated',
  'policy_changed',
  'login',
] as const;

const SEVERITIES = ['info', 'warning', 'error'] as const;

// AuditPage (T611) — operator-facing forensic view of every action
// the control plane records. Reads from GET /api/v1/audit which is
// itself a thin wrapper around audit.SQLiteWriter.Query(), so any
// filter the backend supports is exposed here. Polled on demand
// (Refresh button) rather than on an interval — audit data is
// historical and operators usually care about a specific window.
export function AuditPage() {
  const [eventType, setEventType] = useState('');
  const [severity, setSeverity] = useState('');
  const [actor, setActor] = useState('');
  const [limit, setLimit] = useState('100');

  const params = new URLSearchParams();
  if (eventType) params.set('event_type', eventType);
  if (severity) params.set('severity', severity);
  if (actor) params.set('actor_subject', actor);
  params.set('limit', limit);

  const url = `/api/v1/audit?${params.toString()}`;
  const { data, isLoading, error, refetch, isFetching } = useApi<AuditResponse>(
    ['audit', eventType, severity, actor, limit],
    url,
  );

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Audit Logs</h1>
          <p className="text-sm text-muted-foreground">
            Every API call, pack invocation, vault read, and login the
            control plane has recorded. Filters narrow the SQLite query
            server-side; sensitive payload fields are redacted at write
            time per ADR 010.
          </p>
        </div>
        <Badge variant="outline">
          <FileSearch className="mr-1 h-3 w-3" />
          {data?.count ?? 0} entries
        </Badge>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Filters</CardTitle>
          <CardDescription>
            Narrow the result set by event type, severity, or actor.
            Empty fields match everything.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-5">
            <div className="space-y-1.5">
              <Label htmlFor="filter-event">Event type</Label>
              <select
                id="filter-event"
                value={eventType}
                onChange={(e) => setEventType(e.target.value)}
                className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm focus:outline-none focus:ring-1 focus:ring-ring"
              >
                <option value="">all</option>
                {EVENT_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="filter-severity">Severity</Label>
              <select
                id="filter-severity"
                value={severity}
                onChange={(e) => setSeverity(e.target.value)}
                className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm focus:outline-none focus:ring-1 focus:ring-ring"
              >
                <option value="">all</option>
                {SEVERITIES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="filter-actor">Actor (JWT sub)</Label>
              <Input
                id="filter-actor"
                placeholder="admin"
                value={actor}
                onChange={(e) => setActor(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="filter-limit">Limit</Label>
              <Input
                id="filter-limit"
                type="number"
                min={1}
                max={1000}
                value={limit}
                onChange={(e) => setLimit(e.target.value)}
              />
            </div>
            <div className="flex items-end">
              <Button
                onClick={() => refetch()}
                disabled={isFetching}
                className="w-full"
              >
                <RefreshCw
                  className={`mr-2 h-4 w-4 ${isFetching ? 'animate-spin' : ''}`}
                />
                Refresh
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>

      {error && (
        <Card>
          <CardHeader>
            <CardTitle className="text-destructive">
              Failed to load audit log
            </CardTitle>
            <CardDescription>{error.message}</CardDescription>
          </CardHeader>
          <CardContent className="text-sm text-muted-foreground">
            The audit writer may be running in Discard mode (test/dev
            instances) or the SQLite database is unreachable.
          </CardContent>
        </Card>
      )}

      {isLoading ? (
        <Card>
          <CardContent className="space-y-3 pt-6">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </CardContent>
        </Card>
      ) : (
        !error && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-[140px]">When</TableHead>
                <TableHead>Event</TableHead>
                <TableHead>Actor</TableHead>
                <TableHead>Path</TableHead>
                <TableHead className="w-[80px]">Status</TableHead>
                <TableHead className="w-[90px]">Severity</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {!data?.entries || data.entries.length === 0 ? (
                <TableEmpty colSpan={6}>
                  <ShieldAlert className="mx-auto mb-2 h-8 w-8 text-muted-foreground/50" />
                  No audit entries match the current filters. Try
                  widening the filter set or generating activity via
                  the API.
                </TableEmpty>
              ) : (
                data.entries.map((e) => (
                  <TableRow key={e.id}>
                    <TableCell className="text-xs text-muted-foreground">
                      {formatRelative(e.timestamp)}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {e.event_type}
                    </TableCell>
                    <TableCell className="text-xs">
                      {e.actor_subject ?? '—'}
                      {e.actor_client && (
                        <span className="ml-1 text-muted-foreground">
                          ({e.actor_client})
                        </span>
                      )}
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {e.method && <span className="mr-1">{e.method}</span>}
                      {e.path ?? '—'}
                    </TableCell>
                    <TableCell className="text-xs">
                      <StatusBadge code={e.status_code} />
                    </TableCell>
                    <TableCell>
                      <SeverityBadge severity={e.severity} />
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        )
      )}
    </div>
  );
}

function StatusBadge({ code }: { code: number }) {
  if (code === 0) return <span className="text-muted-foreground">—</span>;
  const v =
    code >= 500
      ? 'destructive'
      : code >= 400
        ? 'warning'
        : code >= 200 && code < 300
          ? 'success'
          : 'outline';
  return <Badge variant={v}>{code}</Badge>;
}

function SeverityBadge({ severity }: { severity: string }) {
  const v =
    severity === 'error'
      ? 'destructive'
      : severity === 'warning'
        ? 'warning'
        : 'outline';
  return <Badge variant={v}>{severity}</Badge>;
}
