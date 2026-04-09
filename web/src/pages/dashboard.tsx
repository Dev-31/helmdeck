import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import {
  Activity,
  Boxes,
  KeyRound,
  type LucideIcon,
  Package,
  Server,
} from 'lucide-react';

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { useApi } from '@/lib/queries';
import { useAuth } from '@/lib/auth';

// DashboardPage (T602) — landing page after login. Shows live
// counts pulled from the panels' own endpoints. Recharts memory
// graph and the activity feed land in T602a once the audit log
// has a read endpoint.

interface PackInfo { name: string }
interface SessionsResponse { sessions?: { id: string }[] }
interface MCPResponse { servers?: { id: string }[] }
interface VaultResponse { count: number }

export function DashboardPage() {
  const { subject } = useAuth();
  const sessions = useApi<SessionsResponse>(['sessions'], '/api/v1/sessions');
  const packs = useApi<PackInfo[]>(['packs'], '/api/v1/packs');
  const mcp = useApi<MCPResponse>(['mcp-servers'], '/api/v1/mcp/servers');
  const vault = useApi<VaultResponse>(['vault-credentials'], '/api/v1/vault/credentials');

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
        <p className="text-sm text-muted-foreground">
          Welcome back, {subject}. Live counts across the helmdeck control
          plane.
        </p>
      </div>

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <StatCard
          to="/sessions"
          label="Active sessions"
          icon={Server}
          value={sessions.data?.sessions?.length}
          loading={sessions.isLoading}
          error={!!sessions.error}
        />
        <StatCard
          to="/packs"
          label="Capability packs"
          icon={Package}
          value={packs.data?.length}
          loading={packs.isLoading}
          error={!!packs.error}
        />
        <StatCard
          to="/mcp"
          label="MCP servers"
          icon={Boxes}
          value={mcp.data?.servers?.length}
          loading={mcp.isLoading}
          error={!!mcp.error}
        />
        <StatCard
          to="/vault"
          label="Vault credentials"
          icon={KeyRound}
          value={vault.data?.count}
          loading={vault.isLoading}
          error={!!vault.error}
        />
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <SystemInfoCard />

        <Card>
          <CardHeader>
            <CardTitle>Resources</CardTitle>
            <CardDescription>Documentation & community</CardDescription>
          </CardHeader>
          <CardContent className="space-y-2 text-sm">
            <div>
              <a
                href="https://github.com/tosin2013/helmdeck"
                className="text-primary hover:underline"
                target="_blank"
                rel="noreferrer"
              >
                GitHub repository
              </a>
            </div>
            <div>
              <a
                href="https://github.com/tosin2013/helmdeck/blob/main/docs/SECURITY-HARDENING.md"
                className="text-primary hover:underline"
                target="_blank"
                rel="noreferrer"
              >
                Security hardening guide
              </a>
            </div>
            <div>
              <a
                href="https://github.com/tosin2013/helmdeck/blob/main/docs/SIDECAR-LANGUAGES.md"
                className="text-primary hover:underline"
                target="_blank"
                rel="noreferrer"
              >
                Sidecar language images
              </a>
            </div>
            <div>
              <a
                href="https://github.com/tosin2013/helmdeck/blob/main/CONTRIBUTING.md"
                className="text-primary hover:underline"
                target="_blank"
                rel="noreferrer"
              >
                Contribution guide (write your own pack)
              </a>
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

interface StatCardProps {
  to: string;
  label: string;
  icon: LucideIcon;
  value?: number;
  loading?: boolean;
  error?: boolean;
}

function StatCard({ to, label, icon: Icon, value, loading, error }: StatCardProps) {
  return (
    <Link to={to}>
      <Card className="transition-colors hover:border-primary/50 hover:bg-accent/30">
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle className="text-sm font-medium text-muted-foreground">{label}</CardTitle>
          <Icon className="h-4 w-4 text-muted-foreground" />
        </CardHeader>
        <CardContent>
          {loading ? (
            <Skeleton className="h-8 w-16" />
          ) : error ? (
            <div className="text-2xl font-bold text-muted-foreground">—</div>
          ) : (
            <div className="text-2xl font-bold">{value ?? 0}</div>
          )}
          <Activity className="mt-1 inline h-3 w-3 text-emerald-400" />{' '}
          <span className="text-xs text-muted-foreground">live</span>
        </CardContent>
      </Card>
    </Link>
  );
}

// SystemInfoCard surfaces operator-facing facts about the running
// control plane: version, signed-in user, auth status, when the
// session was last verified. This is the kind of "is the system
// healthy and which version am I on" view operators want on the
// landing page — distinct from the project-tracking metadata that
// belongs in MILESTONES.md and not in the UI.
function SystemInfoCard() {
  const { subject } = useAuth();
  const version = useApi<{ version: string }>(['version'], '/version', {
    refetchInterval: 30_000,
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>System info</CardTitle>
        <CardDescription>Live status of the control plane</CardDescription>
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        <Row label="Control plane">
          {version.isLoading ? (
            <Skeleton className="h-4 w-20" />
          ) : (
            <span className="font-mono text-xs">{version.data?.version ?? 'unknown'}</span>
          )}
        </Row>
        <Row label="Signed in as">
          <span className="font-mono text-xs">{subject ?? 'unknown'}</span>
        </Row>
        <Row label="Auth">
          <span className="text-xs text-emerald-400">JWT — 12 h session</span>
        </Row>
        <Row label="API base">
          <span className="font-mono text-xs">/api/v1</span>
        </Row>
        <Row label="Time">
          <span className="font-mono text-xs">{new Date().toLocaleString()}</span>
        </Row>
      </CardContent>
    </Card>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-center justify-between">
      <span className="text-muted-foreground">{label}</span>
      {children}
    </div>
  );
}
