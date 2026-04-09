import { PanelStub } from './_stub';

export function ConnectPage() {
  return (
    <PanelStub
      title="Connect Clients"
      description="One-click MCP config snippets for Claude Code, Claude Desktop, OpenClaw, and Gemini CLI."
      hint="Snippets are available today via GET /api/v1/connect/{client} where {client} is one of claude-code, claude-desktop, openclaw, gemini-cli."
    />
  );
}
