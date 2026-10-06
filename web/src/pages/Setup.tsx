import { createMyClient, loadMyClients, setupLinkMyClient } from "../api";
import { type ClientCredential } from "../types";
import { CopyButton, classNames } from "../ui";
import { type FormEvent, useCallback, useEffect, useRef, useState } from "react";

export function SetupPage() {
  const [clients, setClients] = useState<ClientCredential[]>([]);
  const [selected, setSelected] = useState("");
  const [setupLinks, setSetupLinks] = useState<{ urls: Record<string, string>; expires: Date } | null>(null);
  const [tool, setTool] = useState("codex");
  const [label, setLabel] = useState("");
  const [showMint, setShowMint] = useState(false);
  const [loading, setLoading] = useState(true);
  const [revealing, setRevealing] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  const revealVersion = useRef(0);
  const base = window.location.origin;

  const reveal = useCallback(async (id: string) => {
    const version = ++revealVersion.current;
    setSelected(id);
    setSetupLinks(null);
    setRevealing(true);
    setError("");
    try {
      const result = await setupLinkMyClient(id);
      if (version === revealVersion.current) setSetupLinks({ urls: result.setup_urls, expires: new Date(result.nonce_expires_at) });
    } catch (cause) {
      if (version === revealVersion.current) setError(cause instanceof Error ? cause.message : "Unable to load setup. Select the client to retry.");
    } finally {
      if (version === revealVersion.current) setRevealing(false);
    }
  }, []);
  const refresh = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const items = await loadMyClients();
      setClients(items);
      setSelected(current => current || (items.find(client => client.status === "active")?.id ?? ""));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to load clients. Try again.");
    } finally {
      setLoading(false);
    }
  }, []);
  useEffect(() => { void refresh(); return () => { revealVersion.current++; }; }, [refresh]);

  const create = async (event: FormEvent) => {
    event.preventDefault();
    if (creating) return;
    setCreating(true);
    setError("");
    try {
      const result = await createMyClient(label);
      revealVersion.current++;
      await refresh();
      setSelected(result.id);
      setSetupLinks(null);
      setLabel("");
      setShowMint(false);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to create client. Try again.");
    } finally {
      setCreating(false);
    }
  };

  const nonce = (provider: string) => {
    if (!setupLinks) return "························";
    return (setupLinks.urls[provider] || "").split("/").pop() || ".";
  };
  const cliTools: Record<string, { name: string; install: string; oneliner: string; powershell: string; manual: { file: string; url?: string; body?: string; note?: string }[] }> = {
    codex: {
      name: "Codex",
      install: "npm install -g @openai/codex    # or: brew install codex",
      oneliner: `curl -sL "${base}/setup/codex/${nonce("codex")}" | bash`,
      powershell: `irm "${base}/setup/codex/${nonce("codex")}?shell=powershell" | iex`,
      manual: [{ file: "~/.codex/auth.json", url: `${base}/config/codex/${nonce("codex")}` }],
    },
    claude: {
      name: "Claude Code",
      install: "npm install -g @anthropic-ai/claude-code",
      oneliner: `source <(curl -sL "${base}/setup/claude/${nonce("claude")}")`,
      powershell: `irm "${base}/setup/claude/${nonce("claude")}?shell=powershell" | iex`,
      manual: [{ file: "~/.claude/settings.json", url: `${base}/config/claude/${nonce("claude")}` }],
    },
    gemini: {
      name: "Gemini",
      install: "npm install -g @google/gemini-cli    # or: brew install gemini-cli",
      oneliner: `curl -sL "${base}/setup/gemini/${nonce("gemini")}" | bash`,
      powershell: `irm "${base}/setup/gemini/${nonce("gemini")}?shell=powershell" | iex`,
      manual: [{ file: "~/.gemini/oauth_creds.json", url: `${base}/config/gemini/${nonce("gemini")}` }],
    },
    antigravity: {
      name: "Antigravity",
      install: "curl -fsSL https://antigravity.google/cli/install.sh | bash    # or Windows: irm https://antigravity.google/cli/install.ps1 | iex",
      oneliner: `curl -sL "${base}/setup/antigravity/${nonce("antigravity")}" | bash`,
      powershell: `irm "${base}/setup/antigravity/${nonce("antigravity")}?shell=powershell" | iex`,
      manual: [
        { file: "~/.gemini/antigravity-cli/settings.json", body: '{\n  "modelProvider": "gemini"\n}', note: "Merge into the existing file, keeping other keys. GEMINI_API_KEY alone is not enough — modelProvider must be set." },
        { file: "shell profile exports", url: `${base}/config/antigravity/${nonce("antigravity")}`, note: "The one-time JSON returns api_key and base_url. Export GEMINI_API_KEY (api_key) and GOOGLE_GEMINI_BASE_URL (base_url) in your shell profile." },
        { file: "disconnect from the pool", body: 'sed -i.bak \'/# >>> Antigravity Pool Configuration >>>/,/# <<< Antigravity Pool Configuration <<</d\' ~/.zshrc ~/.bashrc\nunset GEMINI_API_KEY GOOGLE_GEMINI_BASE_URL\n# Windows: clear the persisted user-scope vars and the live session\n#   [Environment]::SetEnvironmentVariable(\'GEMINI_API_KEY\', $null, \'User\')\n#   [Environment]::SetEnvironmentVariable(\'GOOGLE_GEMINI_BASE_URL\', $null, \'User\')\n#   Remove-Item Env:GEMINI_API_KEY, Env:GOOGLE_GEMINI_BASE_URL -ErrorAction SilentlyContinue\n# Then remove "modelProvider" from ~/.gemini/antigravity-cli/settings.json to restore Google sign-in.', note: "Optional: revert to the default account sign-in." },
      ],
    },
    grok: {
      name: "Grok",
      install: "npm install -g @xai/grok-cli",
      oneliner: `curl -sL "${base}/setup/grok/${nonce("grok")}" | bash`,
      powershell: `irm "${base}/setup/grok/${nonce("grok")}?shell=powershell" | iex`,
      manual: [{ file: "grok auth", url: `${base}/config/grok/${nonce("grok")}` }],
    },
    "cute-code": {
      name: "Cute Code",
      install: "curl -fsSL https://git.irrigate.cc/pp/cute-code/raw/branch/main/install.sh | bash",
      oneliner: `curl -sL "${base}/setup/cute-code/${nonce("cute-code")}" | bash`,
      powershell: `irm "${base}/setup/cute-code/${nonce("cute-code")}?shell=powershell" | iex`,
      manual: [{ file: "~/.claude/settings.json", url: `${base}/config/cute-code/${nonce("cute-code")}` }],
    },
    pi: {
      name: "Pi",
      install: "npm install -g pi-cli",
      oneliner: `curl -sL "${base}/setup/pi/${nonce("pi")}" | bash`,
      powershell: `irm "${base}/setup/pi/${nonce("pi")}?shell=powershell" | iex`,
      manual: [{ file: "pi models.json", url: `${base}/config/pi/${nonce("pi")}` }],
    },
  };
  const sdkTools: Record<string, { name: string; summary: string; examples: { label: string; code: string }[] }> = {
    anthropic: {
      name: "Anthropic API",
      summary: "Use the pool credential as an Anthropic API key. Claude models route natively; GPT/Kimi/MiniMax/GLM/Xiaomi are translated through /v1/messages. Get the key from the Claude Code config fetch above (access_token).",
      examples: [
        { label: "Python SDK", code: `pip install anthropic\n\nfrom anthropic import Anthropic\nclient = Anthropic(base_url="${base}", api_key="${"<access_token>"}")\nmsg = client.messages.create(model="claude-sonnet-5", max_tokens=1024, messages=[{"role": "user", "content": "hello"}])` },
        { label: "Env + curl", code: `export ANTHROPIC_BASE_URL="${base}"\nexport ANTHROPIC_API_KEY="<access_token>"\n\ncurl "$ANTHROPIC_BASE_URL/v1/messages" \\\n  -H "x-api-key: $ANTHROPIC_API_KEY" \\\n  -H "anthropic-version: 2023-06-01" \\\n  -H "content-type: application/json" \\\n  -d '{"model":"claude-sonnet-5","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}'` },
      ],
    },
    openai: {
      name: "OpenAI SDK",
      summary: "Works with the official OpenAI SDK, Cursor, Continue, Aider, LiteLLM, and any OpenAI-compatible client. Chat Completions and Responses both work; model names route automatically. Get the key from the Codex config fetch above (access_token).",
      examples: [
        { label: "Python SDK", code: `pip install openai\n\nfrom openai import OpenAI\nclient = OpenAI(base_url="${base}/v1", api_key="<access_token>")\nresp = client.responses.create(model="gpt-6-astra", input="hello")` },
        { label: "TypeScript SDK", code: `npm install openai\n\nimport OpenAI from "openai";\nconst client = new OpenAI({ baseURL: "${base}/v1", apiKey: "<access_token>" });\nconst resp = await client.responses.create({ model: "gpt-6-astra", input: "hello" });` },
        { label: "curl", code: `curl "${base}/v1/responses" \\\n  -H "Authorization: Bearer <access_token>" \\\n  -H "content-type: application/json" \\\n  -d '{"model":"gpt-6-astra","input":"hello"}'` },
      ],
    },
  };
  const modelPills = ["claude-sonnet-5", "claude-opus-5", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.4", "kimi-for-coding", "MiniMax-M3", "glm-5.3", "grok-4.5", "opencode-go/longcat-2.0"];

  const activeTool = cliTools[tool];
  const activeSdk = sdkTools[tool];

  return <section className="signal-view setup-page">
    {error && <div className="signal-error" role="alert">{error}</div>}
    <div className="view-title"><h1>Set up a client</h1></div>
    <section className="setup-client-bar" aria-labelledby="setup-client-title">
      <div><span id="setup-client-title">Client</span></div>
      <div className="setup-clients">
      {clients.map((client) => <button key={client.id} className={classNames("client-pill", selected === client.id && "active", client.status !== "active" && "inactive")} disabled={client.status !== "active" || creating} aria-pressed={selected === client.id} onClick={() => { revealVersion.current++; setSelected(client.id); setSetupLinks(null); setRevealing(false); setError(""); }}>{client.label}</button>)}
      {!showMint && clients.length > 0 && <button className="client-pill add" onClick={() => setShowMint(true)}>Add client</button>}
        {showMint && <form className="setup-client-create" onSubmit={create}>
          <input aria-label="Client name" value={label} onChange={(e) => setLabel(e.target.value)} placeholder="Work laptop" maxLength={80} required autoFocus />
          <button className="gold-button" disabled={creating}>{creating ? "Creating…" : "Add client"}</button>
          <button type="button" className="quiet-button" onClick={() => setShowMint(false)}>Cancel</button>
        </form>}
      </div>
    </section>
    {!loading && !error && !clients.some(client => client.status === "active") && !showMint && <div className="empty-state setup-empty"><p>No active clients. Create one to connect a tool.</p><button className="gold-button" onClick={() => setShowMint(true)}>Create a client</button></div>}
    {!loading && (clients.some(client => client.status === "active") || showMint) && <>
      <nav className="tool-tabs" aria-label="Tool to configure">
        {Object.keys(cliTools).map((key) => <button key={key} className={classNames("tab", tool === key && "active")} onClick={() => setTool(key)}>{cliTools[key].name}</button>)}
        <span className="tool-tab-divider" aria-hidden="true" />
        {Object.keys(sdkTools).map((key) => <button key={key} className={classNames("tab", tool === key && "active")} onClick={() => setTool(key)}>{sdkTools[key].name}</button>)}
      </nav>
      {activeTool && <div className="tool-detail">
        <header className="tool-heading"><h2>{activeTool.name}</h2></header>
        <section className="setup-step"><span>1</span><div><h3>Install {activeTool.name}</h3><div className="code-wrapper"><div className="code-block"><pre>{activeTool.install}</pre></div><CopyButton className="copy-btn" text={activeTool.install} /></div></div></section>
        <section className="setup-step"><span>2</span><div><h3>Connect it to Codex Pool</h3>
          {setupLinks
            ? <p className="step-note">One-time link, expires {setupLinks.expires.toLocaleTimeString()}.</p>
            : <div className="setup-generate">
                <button className="gold-button" disabled={!selected || revealing} onClick={() => selected && reveal(selected)}>{revealing ? "Generating…" : "Generate link"}</button>
                {error && selected && <button className="quiet-button" disabled={revealing} onClick={() => reveal(selected)}>Retry</button>}
              </div>}
          {setupLinks && <button className="quiet-button" disabled={!selected || revealing} onClick={() => selected && reveal(selected)}>{revealing ? "Generating…" : "Revoke and generate new"}</button>}
          <p className="step-note">macOS or Linux</p><div className="code-wrapper"><div className="code-block"><pre>{activeTool.oneliner}</pre></div><CopyButton className="copy-btn" text={activeTool.oneliner} disabled={!setupLinks} /></div>
          <p className="step-note">Windows PowerShell</p><div className="code-wrapper"><div className="code-block"><pre>{activeTool.powershell}</pre></div><CopyButton className="copy-btn" text={activeTool.powershell} disabled={!setupLinks} /></div>
          <p className="step-note">Setup connects this client to your user. To use models, add a provider account or ask its owner to grant account access in Sharing. A policy can restrict access already granted.</p>
          <details className="manual-setup"><summary>Configure files manually</summary>{activeTool.manual.every((item) => item.url) ? <p>Fetch the config file directly and place it yourself.</p> : <p>Place each snippet yourself.</p>}{activeTool.manual.map((item) => <div key={item.file} className="manual-item">
            <div className="code-wrapper"><div className="code-block"><pre>{item.body ? `${item.body}\n# → ${item.file}` : `curl -sL "${item.url}"\n# → ${item.file}`}</pre></div><CopyButton className="copy-btn" text={item.body ?? `curl -sL "${item.url}"`} disabled={!item.body && !setupLinks} /></div>
            {item.note && <p className="step-note">{item.note}</p>}
          </div>)}</details>
        </div></section>
      </div>}
      {activeSdk && <div className="tool-detail">
        <header className="tool-heading"><h2>{activeSdk.name}</h2><p>{activeSdk.summary}</p></header>
        <div className="api-cards">
          <div className="api-card"><div className="api-card-title">Base URL</div><code>{tool === "openai" ? `${base}/v1` : base}</code></div>
          <div className="api-card"><div className="api-card-title">Authentication</div><code>{tool === "openai" ? "Authorization: Bearer <token>" : "x-api-key: <token>"}</code></div>
        </div>
        {activeSdk.examples.map((ex) => <section className="sdk-example" key={ex.label}><h3>{ex.label}</h3><div className="code-wrapper"><div className="code-block"><pre>{ex.code}</pre></div><CopyButton className="copy-btn" text={ex.code} /></div></section>)}
        <h3 className="models-heading">Model IDs</h3>
        <div className="model-pills">{modelPills.map((m) => <span key={m} className="model-pill">{m}</span>)}</div>
      </div>}
    </>}
  </section>;
}
