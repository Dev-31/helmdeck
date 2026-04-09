import { PanelStub } from './_stub';

export function ProvidersPage() {
  return (
    <PanelStub
      title="AI Providers"
      description="Configure provider keys, test connections, and edit fallback chain rules."
      hint="Provider keys can be managed today via the /api/v1/providers/keys CRUD endpoints. The OpenAI-compatible facade lives at /v1/chat/completions and /v1/models."
    />
  );
}
