import { Activity, Key, KeyRound, TrendingUp } from 'lucide-react';

import { Badge } from '@/components/ui/badge';
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
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

interface ProviderKey {
  id: string;
  provider: string;
  label: string;
  fingerprint: string;
  last4: string;
  created_at: string;
  updated_at: string;
  last_used_at?: string;
}

interface ProviderStatsRow {
  provider: string;
  model: string;
  total: number;
  success: number;
  errors: number;
  success_rate: number;
  avg_latency_ms: number;
  last_seen: string;
}

interface ProviderStatsResponse {
  window: string;
  from: string;
  rows: ProviderStatsRow[];
  count: number;
}

// ProvidersPage (T604 + T607) — operator view of every AI provider
// key in the keystore (T604) AND the per-model success-rate rollup
// for the last 24 hours (T607). Both come from
// /api/v1/providers/{keys,stats}; the panel composes them into one
// page so operators can see "this key drives this model and it has
// a 75% success rate" without paging between two screens.
export function ProvidersPage() {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">AI Providers</h1>
        <p className="text-sm text-muted-foreground">
          API keys for the OpenAI-compatible facade at{' '}
          <code className="rounded bg-muted px-1.5 py-0.5">
            /v1/chat/completions
          </code>{' '}
          plus per-model success rates for the last 24 hours. Keys are
          stored AES-256-GCM encrypted in the keystore (ADR 011); only
          fingerprint and last 4 chars are shown here.
        </p>
      </div>

      <KeysSection />
      <StatsSection />
    </div>
  );
}

function KeysSection() {
  const { data, isLoading, error } = useApi<ProviderKey[]>(
    ['providers', 'keys'],
    '/api/v1/providers/keys',
    { refetchInterval: 15_000 },
  );

  return (
    <section className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold tracking-tight">
          Provider keys
        </h2>
        <Badge variant="outline">
          <KeyRound className="mr-1 h-3 w-3" />
          {data?.length ?? 0} keys
        </Badge>
      </div>

      {error && (
        <Card>
          <CardHeader>
            <CardTitle className="text-destructive">
              Failed to load provider keys
            </CardTitle>
            <CardDescription>{error.message}</CardDescription>
          </CardHeader>
          <CardContent className="text-sm text-muted-foreground">
            The keystore may be unavailable or{' '}
            <code className="rounded bg-muted px-1.5 py-0.5">
              HELMDECK_KEYSTORE_KEY
            </code>{' '}
            was not set when the control plane started.
          </CardContent>
        </Card>
      )}

      {isLoading ? (
        <Card>
          <CardContent className="space-y-3 pt-6">
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
                <TableHead>Provider</TableHead>
                <TableHead>Label</TableHead>
                <TableHead>Fingerprint</TableHead>
                <TableHead>Last 4</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Last used</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {!data || data.length === 0 ? (
                <TableEmpty colSpan={6}>
                  <Key className="mx-auto mb-2 h-8 w-8 text-muted-foreground/50" />
                  No provider keys configured. Add one via{' '}
                  <code className="rounded bg-muted px-1.5 py-0.5">
                    POST /api/v1/providers/keys
                  </code>
                  . The Add Key modal lands in T604a.
                </TableEmpty>
              ) : (
                data.map((k) => (
                  <TableRow key={k.id}>
                    <TableCell>
                      <Badge variant="outline">{k.provider}</Badge>
                    </TableCell>
                    <TableCell>{k.label || '—'}</TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {k.fingerprint.slice(0, 12)}…
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      …{k.last4}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {formatRelative(k.created_at)}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {k.last_used_at ? formatRelative(k.last_used_at) : 'never'}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        )
      )}
    </section>
  );
}

// StatsSection (T607) — per-(provider, model) success-rate rollup.
// Reads /api/v1/providers/stats?window=24h (the endpoint clamps to
// 30d max). The aggregation table is populated by every dispatch
// the gateway routes; rows show up here as soon as the first call
// lands. Empty state on a fresh install nudges the operator at the
// /v1/chat/completions endpoint.
function StatsSection() {
  const { data, isLoading, error } = useApi<ProviderStatsResponse>(
    ['providers', 'stats'],
    '/api/v1/providers/stats?window=24h',
    { refetchInterval: 30_000 },
  );

  return (
    <section className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold tracking-tight">
          Model success rates
        </h2>
        <Badge variant="outline">
          <TrendingUp className="mr-1 h-3 w-3" />
          last 24h
        </Badge>
      </div>

      {error && (
        <Card>
          <CardHeader>
            <CardTitle className="text-destructive">
              Failed to load model stats
            </CardTitle>
            <CardDescription>{error.message}</CardDescription>
          </CardHeader>
        </Card>
      )}

      {isLoading ? (
        <Card>
          <CardContent className="space-y-3 pt-6">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </CardContent>
        </Card>
      ) : (
        !error && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Provider</TableHead>
                <TableHead>Model</TableHead>
                <TableHead className="text-right">Calls</TableHead>
                <TableHead className="text-right">Success</TableHead>
                <TableHead className="text-right">Errors</TableHead>
                <TableHead>Success rate</TableHead>
                <TableHead className="text-right">Avg latency</TableHead>
                <TableHead>Last seen</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {!data?.rows || data.rows.length === 0 ? (
                <TableEmpty colSpan={8}>
                  <Activity className="mx-auto mb-2 h-8 w-8 text-muted-foreground/50" />
                  No model calls in the last 24 hours. Make a request
                  to{' '}
                  <code className="rounded bg-muted px-1.5 py-0.5">
                    POST /v1/chat/completions
                  </code>{' '}
                  and refresh.
                </TableEmpty>
              ) : (
                data.rows.map((r) => (
                  <TableRow key={`${r.provider}/${r.model}`}>
                    <TableCell>
                      <Badge variant="outline">{r.provider}</Badge>
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {r.model}
                    </TableCell>
                    <TableCell className="text-right text-xs">
                      {r.total}
                    </TableCell>
                    <TableCell className="text-right text-xs text-muted-foreground">
                      {r.success}
                    </TableCell>
                    <TableCell className="text-right text-xs text-muted-foreground">
                      {r.errors}
                    </TableCell>
                    <TableCell>
                      <SuccessRateBar rate={r.success_rate} />
                    </TableCell>
                    <TableCell className="text-right text-xs text-muted-foreground">
                      {r.avg_latency_ms} ms
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {formatRelative(r.last_seen)}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        )
      )}
    </section>
  );
}

function SuccessRateBar({ rate }: { rate: number }) {
  const pct = Math.round(rate * 100);
  const color =
    rate >= 0.99
      ? 'bg-emerald-500'
      : rate >= 0.9
        ? 'bg-amber-500'
        : 'bg-rose-500';
  return (
    <div className="flex items-center gap-2">
      <div className="relative h-2 w-24 overflow-hidden rounded bg-muted">
        <div
          className={`absolute inset-y-0 left-0 ${color}`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <span className="text-xs font-mono text-muted-foreground">{pct}%</span>
    </div>
  );
}
