import { Construction } from 'lucide-react';

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';

// PanelStub is the placeholder shape used by panels whose data is
// already available via the REST API but whose UI hasn't shipped
// yet. Operator-facing copy stays generic — no task IDs, no
// roadmap references. Internal project tracking lives in
// docs/MILESTONES.md.

interface PanelStubProps {
  title: string;
  description: string;
  hint?: string;
}

export function PanelStub({ title, description, hint }: PanelStubProps) {
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        <p className="text-sm text-muted-foreground">{description}</p>
      </div>
      <Card>
        <CardHeader>
          <div className="flex items-center gap-3">
            <div className="rounded-md bg-muted p-2">
              <Construction className="h-5 w-5 text-muted-foreground" />
            </div>
            <div>
              <CardTitle>UI in development</CardTitle>
              <CardDescription>
                The data for this panel is available via the REST API today.
                The visual interface lands in a follow-up release.
              </CardDescription>
            </div>
          </div>
        </CardHeader>
        {hint && (
          <CardContent>
            <p className="text-sm text-muted-foreground">{hint}</p>
          </CardContent>
        )}
      </Card>
    </div>
  );
}
