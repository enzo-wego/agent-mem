import { useEffect, useState } from 'react'
import { fetchSettings, updateSettings, type Settings } from '../api'

export function SettingsPage() {
  const [settings, setSettings] = useState<Settings | null>(null)
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  const [toast, setToast] = useState<{ type: 'ok' | 'err'; msg: string } | null>(null)
  const [showKey, setShowKey] = useState(false)
  const [newKey, setNewKey] = useState('')
  const [showGoogleKey, setShowGoogleKey] = useState(false)
  const [newGoogleKey, setNewGoogleKey] = useState('')
  const [showAnthropicKey, setShowAnthropicKey] = useState(false)
  const [newAnthropicKey, setNewAnthropicKey] = useState('')

  useEffect(() => {
    fetchSettings()
      .then(setSettings)
      .catch(() => setError('Failed to load settings'))
  }, [])

  const save = async (partial: Partial<Settings>) => {
    setSaving(true)
    setToast(null)
    try {
      const updated = await updateSettings(partial)
      setSettings(updated)
      setToast({ type: 'ok', msg: 'Saved' })
    } catch (e: any) {
      setToast({ type: 'err', msg: e.message || 'Save failed' })
    } finally {
      setSaving(false)
    }
  }

  if (error) return <p className="text-red-500">{error}</p>
  if (!settings) return <p className="text-gray-500 text-sm">Loading...</p>

  return (
    <div className="space-y-6">
      {toast && (
        <div className={`text-sm px-4 py-2 rounded-md ${toast.type === 'ok' ? 'bg-green-50 text-green-700 dark:bg-green-900/30 dark:text-green-300' : 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'}`}>
          {toast.msg}
        </div>
      )}

      {/* Gemini */}
      <Section title="Gemini">
        <Field label="Provider" hint="LLM backend for generation, describe, and embeddings. 'openrouter' (default) uses the OpenRouter key; 'google' uses the Google API key — the fallback when OpenRouter is out of quota. Observation extraction switches immediately; the graph pipeline picks up the new provider after a worker restart.">
          <SelectField
            value={settings.llm_provider || 'openrouter'}
            options={PROVIDER_OPTIONS}
            saving={saving}
            onSave={(v) => save({ llm_provider: v })}
          />
        </Field>
        <Field label="OpenRouter API Key" hint="OpenRouter key (sk-or…) from openrouter.ai/keys. Used when provider is 'openrouter'.">
          <div className="flex gap-2">
            <input
              type={showKey ? 'text' : 'password'}
              placeholder={settings.gemini_api_key || 'Not set'}
              value={newKey}
              onChange={(e) => setNewKey(e.target.value)}
              className={inputCls}
            />
            <button onClick={() => setShowKey(!showKey)} className={btnSecondary}>
              {showKey ? 'Hide' : 'Show'}
            </button>
            <button
              disabled={saving || !newKey}
              onClick={() => { save({ gemini_api_key: newKey }); setNewKey('') }}
              className={btnPrimary}
            >
              Update Key
            </button>
          </div>
        </Field>
        <Field label="Google API Key" hint="Google AI Studio key (AIza…) from aistudio.google.com. Used when provider is 'google'. Pre-set it so failover is a one-click provider switch.">
          <div className="flex gap-2">
            <input
              type={showGoogleKey ? 'text' : 'password'}
              placeholder={settings.google_api_key || 'Not set'}
              value={newGoogleKey}
              onChange={(e) => setNewGoogleKey(e.target.value)}
              className={inputCls}
            />
            <button onClick={() => setShowGoogleKey(!showGoogleKey)} className={btnSecondary}>
              {showGoogleKey ? 'Hide' : 'Show'}
            </button>
            <button
              disabled={saving || !newGoogleKey}
              onClick={() => { save({ google_api_key: newGoogleKey }); setNewGoogleKey('') }}
              className={btnPrimary}
            >
              Update Key
            </button>
          </div>
        </Field>
        <Field label="Flat-Memory Model" hint="Model for flat memory (observation extraction, session summaries). Any OpenRouter model id, e.g. google/gemini-2.5-flash, anthropic/claude-haiku-4.5, openai/gpt-5.6-luna. On the google provider use a bare Gemini id (e.g. gemini-2.5-flash).">
          <EditableField
            value={settings.gemini_model}
            saving={saving}
            onSave={(v) => save({ gemini_model: v })}
            placeholder="google/gemini-2.5-flash"
          />
        </Field>
        <Field label="Graph-Memory Model" hint="Model for graph memory (attachment describe, topic linking). Empty = use the flat-memory model. Same id rules as above; a non-Google id here only works on the openrouter provider.">
          <EditableField
            value={settings.graph_gemini_model}
            saving={saving}
            onSave={(v) => save({ graph_gemini_model: v })}
            placeholder="google/gemini-3.5-flash"
          />
        </Field>
        <Field label="Embedding Model" hint="Used for semantic search embeddings. gemini-embedding-001 is the same model on both providers — the google/ prefix is added/stripped automatically per provider, so you don't change this when switching provider. Changing to a different model requires re-embedding all data.">
          <SelectField
            value={settings.gemini_embedding_model}
            options={EMBEDDING_MODELS}
            saving={saving}
            onSave={(v) => save({ gemini_embedding_model: v })}
          />
        </Field>
        <Field label="Embedding Dimensions" hint="Vector size. Must match your database column. Changing this requires re-embedding all data.">
          <SelectField
            value={String(settings.gemini_embedding_dims)}
            options={EMBEDDING_DIMS}
            saving={saving}
            onSave={(v) => save({ gemini_embedding_dims: Number(v) })}
          />
        </Field>
      </Section>

      {/* Claude (graph summaries) */}
      <Section title="Claude (graph summaries)">
        <Field label="API Key" hint="Anthropic key (sk-ant…). When set, graph summaries (thread / cluster / feature) run on Claude instead of Gemini. Takes effect after a worker restart. NOTE: if AGENT_MEM_ANTHROPIC_API_KEY is set in the VPS env, it overrides this on restart — unset it there to make the dashboard authoritative.">
          <div className="flex gap-2">
            <input
              type={showAnthropicKey ? 'text' : 'password'}
              placeholder={settings.anthropic_api_key || 'Not set'}
              value={newAnthropicKey}
              onChange={(e) => setNewAnthropicKey(e.target.value)}
              className={inputCls}
            />
            <button onClick={() => setShowAnthropicKey(!showAnthropicKey)} className={btnSecondary}>
              {showAnthropicKey ? 'Hide' : 'Show'}
            </button>
            <button
              disabled={saving || !newAnthropicKey}
              onClick={() => { save({ anthropic_api_key: newAnthropicKey }); setNewAnthropicKey('') }}
              className={btnPrimary}
            >
              Update Key
            </button>
          </div>
        </Field>
        <Field label="Model" hint="Claude model for graph summaries. Switch to Haiku for cheaper/faster summaries. Takes effect after a worker restart.">
          <SelectField
            value={settings.anthropic_model || 'claude-sonnet-5'}
            options={ANTHROPIC_MODELS}
            saving={saving}
            onSave={(v) => save({ anthropic_model: v })}
          />
        </Field>
      </Section>

      {/* Projects */}
      <Section title="Projects">
        <Field label="Allowed Projects" hint="Comma-separated whitelist. If set, only these projects are processed. Leave both empty to allow all projects.">
          <EditableField value={settings.allowed_projects} saving={saving} onSave={(v) => save({ allowed_projects: v })} placeholder="e.g. my-project,other-project" />
        </Field>
        <Field label="Ignored Projects" hint="Comma-separated blacklist. Ignored if whitelist is set. Leave both empty to allow all projects.">
          <EditableField value={settings.ignored_projects} saving={saving} onSave={(v) => save({ ignored_projects: v })} placeholder="e.g. test-project,scratch" />
        </Field>
        <Field label="Skip Tools" hint="Claude Code tools to ignore during observation extraction. Skipped tools won't generate memories.">
          <MultiCheckField
            value={settings.skip_tools}
            options={SKIP_TOOLS}
            saving={saving}
            onSave={(v) => save({ skip_tools: v })}
          />
        </Field>
      </Section>

      {/* Context */}
      <Section title="Context Window">
        <Field label="Observations" hint="Number of recent observations (edits, tool uses) injected into each new Claude session. More = broader history, more tokens.">
          <EditableField value={String(settings.context_observations)} saving={saving} onSave={(v) => save({ context_observations: Number(v) })} />
        </Field>
        <Field label="Full Count" hint="How many observations get their full narrative expanded. The rest are shown as one-line table rows.">
          <EditableField value={String(settings.context_full_count)} saving={saving} onSave={(v) => save({ context_full_count: Number(v) })} />
        </Field>
        <Field label="Session Count" hint="Number of past session summaries included. Each summary describes what was requested, completed, and learned.">
          <EditableField value={String(settings.context_session_count)} saving={saving} onSave={(v) => save({ context_session_count: Number(v) })} />
        </Field>
      </Section>

      {/* General */}
      <Section title="General">
        <Field label="Log Level">
          <SelectField
            value={settings.log_level}
            options={LOG_LEVELS}
            saving={saving}
            onSave={(v) => save({ log_level: v })}
          />
        </Field>
      </Section>

    </div>
  )
}

// --- Constants ---

const PROVIDER_OPTIONS = [
  { value: 'openrouter', label: 'OpenRouter (default)' },
  { value: 'google', label: 'Google Gemini API (fallback)' },
]

const ANTHROPIC_MODELS = [
  { value: 'claude-sonnet-5', label: 'Claude Sonnet 5 (default)' },
  { value: 'claude-haiku-4.5', label: 'Claude Haiku 4.5 (cheaper, faster)' },
  { value: 'claude-opus-4-8', label: 'Claude Opus 4.8 (highest quality)' },
]

// Stored with the google/ namespace; the client strips it automatically on the
// google provider, so one value works for both providers.
const EMBEDDING_MODELS = [
  { value: 'google/gemini-embedding-001', label: 'gemini-embedding-001 (recommended)' },
  { value: 'google/text-embedding-004', label: 'text-embedding-004 (legacy, 768-dim)' },
]

const EMBEDDING_DIMS = [
  { value: '256', label: '256' },
  { value: '384', label: '384' },
  { value: '512', label: '512' },
  { value: '768', label: '768 (default)' },
  { value: '1024', label: '1024' },
  { value: '3072', label: '3072 (max)' },
]

const SKIP_TOOLS = [
  { value: 'Read', label: 'Read', desc: 'File reading' },
  { value: 'Write', label: 'Write', desc: 'File creation' },
  { value: 'Edit', label: 'Edit', desc: 'File editing' },
  { value: 'Bash', label: 'Bash', desc: 'Shell commands' },
  { value: 'Glob', label: 'Glob', desc: 'File pattern search' },
  { value: 'Grep', label: 'Grep', desc: 'Content search' },
  { value: 'Agent', label: 'Agent', desc: 'Sub-agent tasks' },
  { value: 'WebSearch', label: 'WebSearch', desc: 'Web searching' },
  { value: 'WebFetch', label: 'WebFetch', desc: 'URL fetching' },
  { value: 'NotebookEdit', label: 'NotebookEdit', desc: 'Jupyter notebooks' },
  { value: 'ListMcpResourcesTool', label: 'ListMcpResourcesTool', desc: 'MCP resource listing' },
  { value: 'SlashCommand', label: 'SlashCommand', desc: 'Slash command execution' },
  { value: 'TodoWrite', label: 'TodoWrite', desc: 'Task management' },
  { value: 'AskFollowupQuestion', label: 'AskFollowupQuestion', desc: 'User questions' },
]

const LOG_LEVELS = [
  { value: 'trace', label: 'Trace' },
  { value: 'debug', label: 'Debug' },
  { value: 'info', label: 'Info (default)' },
  { value: 'warn', label: 'Warn' },
  { value: 'error', label: 'Error' },
]

// --- Subcomponents ---

const inputCls = 'flex-1 px-3 py-1.5 text-sm border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 focus:outline-none focus:ring-2 focus:ring-blue-500'
const selectCls = 'flex-1 px-3 py-1.5 text-sm border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 focus:outline-none focus:ring-2 focus:ring-blue-500'
const btnPrimary = 'px-3 py-1.5 text-sm font-medium rounded-md bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50'
const btnSecondary = 'px-3 py-1.5 text-sm font-medium rounded-md border border-gray-300 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-700'

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
      <h3 className="font-semibold mb-4">{title}</h3>
      <div className="space-y-4">{children}</div>
    </div>
  )
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <div>
      <label className="block text-sm text-gray-500 dark:text-gray-400 mb-1">{label}</label>
      {hint && <p className="text-xs text-gray-400 dark:text-gray-500 mb-1">{hint}</p>}
      {children}
    </div>
  )
}

function SelectField({
  value,
  options,
  saving,
  onSave,
}: {
  value: string
  options: { value: string; label: string }[]
  saving: boolean
  onSave: (v: string) => void
}) {
  const [local, setLocal] = useState(value)
  const dirty = local !== value

  useEffect(() => { setLocal(value) }, [value])

  return (
    <div className="flex gap-2">
      <select
        value={local}
        onChange={(e) => setLocal(e.target.value)}
        className={selectCls}
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>{o.label}</option>
        ))}
        {/* Show current value if not in the list */}
        {!options.some((o) => o.value === local) && (
          <option value={local}>{local} (custom)</option>
        )}
      </select>
      {dirty && (
        <button disabled={saving} onClick={() => onSave(local)} className={btnPrimary}>
          Save
        </button>
      )}
    </div>
  )
}

function MultiCheckField({
  value,
  options,
  saving,
  onSave,
}: {
  value: string
  options: { value: string; label: string; desc: string }[]
  saving: boolean
  onSave: (v: string) => void
}) {
  const parse = (s: string) => s.split(',').map((v) => v.trim()).filter(Boolean)
  const [selected, setSelected] = useState<Set<string>>(new Set(parse(value)))
  const [customInput, setCustomInput] = useState('')

  useEffect(() => { setSelected(new Set(parse(value))) }, [value])

  const knownValues = new Set(options.map((o) => o.value))
  const customTools = Array.from(selected).filter((v) => !knownValues.has(v))

  const toggle = (v: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(v)) next.delete(v)
      else next.add(v)
      return next
    })
  }

  const addCustom = () => {
    const name = customInput.trim()
    if (name && !selected.has(name)) {
      setSelected((prev) => new Set([...prev, name]))
      setCustomInput('')
    }
  }

  const currentValue = Array.from(selected).join(',')
  const dirty = currentValue !== value

  return (
    <div>
      {/* Built-in tools */}
      <p className="text-xs text-gray-400 dark:text-gray-500 mb-1.5">Built-in tools</p>
      <div className="grid grid-cols-2 gap-2">
        {options.map((o) => (
          <label
            key={o.value}
            className={`flex items-center gap-2 px-3 py-2 rounded-md border text-sm cursor-pointer transition-colors ${
              selected.has(o.value)
                ? 'border-blue-500 bg-blue-50 dark:bg-blue-900/20 text-blue-700 dark:text-blue-300'
                : 'border-gray-200 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700/50'
            }`}
          >
            <input
              type="checkbox"
              checked={selected.has(o.value)}
              onChange={() => toggle(o.value)}
              className="rounded border-gray-300 text-blue-600 focus:ring-blue-500"
            />
            <div>
              <span className="font-medium">{o.label}</span>
              <span className="text-xs text-gray-400 dark:text-gray-500 ml-1.5">{o.desc}</span>
            </div>
          </label>
        ))}
      </div>

      {/* Custom / MCP tools */}
      <p className="text-xs text-gray-400 dark:text-gray-500 mt-3 mb-1.5">Custom / MCP tools</p>
      {customTools.length > 0 && (
        <div className="flex flex-wrap gap-1.5 mb-2">
          {customTools.map((t) => (
            <span
              key={t}
              className="inline-flex items-center gap-1 px-2 py-1 rounded-md text-xs font-mono bg-blue-50 dark:bg-blue-900/20 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800"
            >
              {t}
              <button
                onClick={() => toggle(t)}
                className="ml-0.5 hover:text-red-500 font-sans font-bold"
                title="Remove"
              >
                x
              </button>
            </span>
          ))}
        </div>
      )}
      <div className="flex gap-2">
        <input
          type="text"
          value={customInput}
          onChange={(e) => setCustomInput(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); addCustom() } }}
          placeholder="e.g. mcp__slack__send_message"
          className={inputCls}
        />
        <button
          onClick={addCustom}
          disabled={!customInput.trim()}
          className={btnSecondary}
        >
          Add
        </button>
      </div>

      {dirty && (
        <button
          disabled={saving}
          onClick={() => onSave(currentValue)}
          className={`${btnPrimary} mt-3`}
        >
          Save
        </button>
      )}
    </div>
  )
}

function EditableField({
  value,
  saving,
  onSave,
  placeholder,
}: {
  value: string
  saving: boolean
  onSave: (v: string) => void
  placeholder?: string
}) {
  const [local, setLocal] = useState(value)
  const dirty = local !== value

  // Sync when upstream changes after save.
  useEffect(() => { setLocal(value) }, [value])

  return (
    <div className="flex gap-2">
      <input
        type="text"
        value={local}
        onChange={(e) => setLocal(e.target.value)}
        placeholder={placeholder}
        className={inputCls}
      />
      {dirty && (
        <button disabled={saving} onClick={() => onSave(local)} className={btnPrimary}>
          Save
        </button>
      )}
    </div>
  )
}
