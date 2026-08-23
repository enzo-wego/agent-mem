import { useEffect, useState } from 'react'
import {
  fetchSettings,
  updateSettings,
  fetchChannels,
  fetchChannelFilters,
  saveChannelFilters,
  fetchGatewayHealth,
  fetchGatewayConfig,
  updateGatewayConfig,
  getOpenRouterUsage,
  type Settings,
  type ChannelCount,
  type ChannelFilters,
  type GatewayHealth,
  type GatewayConfig,
  type OpenRouterUsage,
} from '../api'

export function SettingsPage() {
  const [settings, setSettings] = useState<Settings | null>(null)
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  const [toast, setToast] = useState<{ type: 'ok' | 'err'; msg: string } | null>(null)
  const [newGatewayKey, setNewGatewayKey] = useState('')

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
    } catch (error: unknown) {
      setToast({ type: 'err', msg: error instanceof Error ? error.message : 'Save failed' })
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

      {/* Processing pause — operational kill switch, kept at the top so it is
          findable when something is actively going wrong. */}
      <Section title="Processing">
        <Field
          label="Pause processing"
          hint="Stop claiming jobs while keeping the API up. Ingest still accepts webhooks and queues work — nothing is sent to an LLM, so this costs nothing while a budget is exhausted. Unpause and the backlog drains. Takes effect within ~5s, no restart. Prefer this over stopping the worker: a stopped worker also stops the API, and inbound Slack webhooks are lost rather than queued."
        >
          <label className="flex items-center gap-2 cursor-pointer">
            <input
              type="checkbox"
              checked={!!settings.processing_paused}
              disabled={saving}
              onChange={(e) => save({ processing_paused: e.target.checked })}
              className="rounded border-gray-300 text-blue-600 focus:ring-blue-500"
            />
            <span className={`text-sm font-medium ${settings.processing_paused ? 'text-amber-600 dark:text-amber-400' : 'text-gray-500 dark:text-gray-400'}`}>
              {settings.processing_paused ? 'PAUSED — queueing only, nothing processed' : 'Running'}
            </span>
          </label>
        </Field>
      </Section>

      {/* Claude via llm-gateway — the sole LLM egress. agent-mem holds no
          provider keys or model names: which backend serves each tier, model
          choice and failover all live in llm-gateway. There is deliberately no
          Anthropic API key field — a metered key has no spend ceiling and one
          amplification bug spent ~$11/hour through it; a subscription seat
          rate-limits instead. */}
      <Section title="llm-gateway (all LLM calls)">
        <Field label="Gateway URL" hint="e.g. http://172.18.0.1:8750 — the Docker bridge, NOT localhost (the worker is containerised). Every LLM call goes through the gateway: graph summaries and the topic judge, flat-memory observations and session summaries, attachment descriptions, and all embeddings. Leave EMPTY to turn LLM processing off entirely — there is no direct-provider fallback in agent-mem, so observation extraction and summaries are simply skipped. Takes effect on save, no restart.">
          <EditableField
            value={settings.llm_gateway_url}
            saving={saving}
            onSave={(v) => save({ llm_gateway_url: v })}
            placeholder="Empty = off (no LLM processing)"
          />
        </Field>
        <Field label="Gateway API Key" hint="The gateway's LLM_GATEWAY_API_KEY — its own inbound auth, not an Anthropic key. Without it the gateway 401s and every summary silently stops.">
          <div className="flex gap-2">
            <input
              type="password"
              placeholder={settings.llm_gateway_api_key || 'Not set'}
              value={newGatewayKey}
              onChange={(e) => setNewGatewayKey(e.target.value)}
              className={inputCls}
            />
            <button
              disabled={saving || !newGatewayKey}
              onClick={() => { save({ llm_gateway_api_key: newGatewayKey }); setNewGatewayKey('') }}
              className={btnPrimary}
            >
              Update Key
            </button>
          </div>
        </Field>
        <Field label="Embedding Dimensions" hint="Fixed by the database schema. Changing either width requires a schema migration plus a full re-embed of the affected corpus.">
          <p className="text-sm font-mono text-gray-700 dark:text-gray-300">
            768 (flat) / 3072 (graph)
          </p>
        </Field>
        <GatewayPanel />
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

      {/* Channel Filters (graph memory) */}
      <ChannelFiltersSection />

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

// --- llm-gateway status + gateway-owned configuration ---

function GatewayPanel() {
  const [healthData, setHealthData] = useState<GatewayHealth | null>(null)
  const [gatewayConfig, setGatewayConfig] = useState<GatewayConfig | null>(null)
  const [openRouterUsage, setOpenRouterUsage] = useState<OpenRouterUsage | null>(null)
  const [budgetDraft, setBudgetDraft] = useState('')
  const [loadError, setLoadError] = useState('')
  const [saving, setSaving] = useState(false)
  const [notice, setNotice] = useState<{ type: 'ok' | 'err'; msg: string } | null>(null)

  const load = async () => {
    setLoadError('')
    const [health, usage, configResult] = await Promise.all([
      fetchGatewayHealth().catch((error: Error) => ({
        available: false,
        error: error.message || 'Gateway health request failed',
      } as GatewayHealth)),
      getOpenRouterUsage().catch(() => null),
      fetchGatewayConfig()
        .then((config) => ({ config, error: '' }))
        .catch((error: Error) => ({ config: null, error: error.message || 'Gateway configuration request failed' })),
    ])

    setHealthData(health)
    setOpenRouterUsage(usage)
    setGatewayConfig(configResult.config)
    if (configResult.config) {
      setBudgetDraft(String(configResult.config.MAX_BUDGET_USD))
    } else {
      setBudgetDraft('')
      setLoadError(configResult.error)
    }
  }

  useEffect(() => { void load() }, [])

  const saveUpdate = async (
    partial: Parameters<typeof updateGatewayConfig>[0],
    successMessage: string,
  ) => {
    setSaving(true)
    setNotice(null)
    try {
      await updateGatewayConfig(partial)
      await load()
      setNotice({ type: 'ok', msg: successMessage })
    } catch (error: unknown) {
      setNotice({ type: 'err', msg: error instanceof Error ? error.message : 'Gateway configuration update failed' })
    } finally {
      setSaving(false)
    }
  }

  const h = healthData?.health || {}
  const seat = h.seat || {}
  const openRouterRemaining = openRouterUsage?.available
    ? openRouterUsage.limit_remaining ?? (
      typeof openRouterUsage.limit === 'number' && typeof openRouterUsage.usage === 'number'
        ? openRouterUsage.limit - openRouterUsage.usage
        : null
    )
    : null
  const budget = Number(budgetDraft)
  const budgetValid = budgetDraft.trim() !== '' && Number.isFinite(budget) && budget > 0

  const tiers = gatewayConfig ? ([
    {
      key: 'BACKEND_CHEAP',
      label: 'Cheap',
      description: 'Observation extraction, session summaries, and high-volume topic-link confirmation.',
      claudeModel: gatewayConfig.MODEL_CHEAP,
      openRouterModel: gatewayConfig.OR_MODEL_CHEAP,
    },
    {
      key: 'BACKEND_SUMMARY',
      label: 'Summary',
      description: 'Thread and cluster summaries, hot-topic detection, and feature-entity derivation.',
      claudeModel: gatewayConfig.MODEL_SUMMARY,
      openRouterModel: gatewayConfig.OR_MODEL_SUMMARY,
    },
    {
      key: 'BACKEND_DESCRIBE',
      label: 'Describe',
      description: 'Multimodal image and PDF attachment descriptions.',
      claudeModel: gatewayConfig.MODEL_CHEAP,
      openRouterModel: gatewayConfig.OR_MODEL_DESCRIBE || gatewayConfig.OR_MODEL_SUMMARY,
    },
  ] as const) : []

  return (
    <>
      <Field label="Gateway Status" hint="Read-only view of llm-gateway /health — backend per tier, models in use, and Claude-seat availability.">
        {!healthData ? (
          <p className="text-xs text-gray-500">Loading…</p>
        ) : !healthData.available ? (
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <span className="px-1.5 py-0.5 rounded bg-amber-50 dark:bg-amber-900/30 text-amber-700 dark:text-amber-300">unavailable</span>
            <span className="text-gray-500">{healthData.error || 'gateway not reachable'}</span>
            <button onClick={() => { void load() }} className={btnSecondary}>Refresh</button>
          </div>
        ) : (
          <div className="space-y-1 text-xs">
            <div className="flex flex-wrap items-center gap-2">
              <span className={seat.available ? 'text-green-600 dark:text-green-400' : 'text-red-600 dark:text-red-400'}>●</span>
              <span>Claude seat {seat.available ? 'available' : 'blocked'}</span>
              {openRouterRemaining !== null && Number.isFinite(openRouterRemaining) && typeof openRouterUsage?.limit === 'number' && (
                <>
                  <span className="text-gray-400">·</span>
                  <span>OpenRouter ${openRouterRemaining.toFixed(2)} left of ${openRouterUsage.limit}</span>
                </>
              )}
              {seat.blocked_until && <span className="text-gray-400">until {new Date(seat.blocked_until).toLocaleString()}</span>}
            </div>
            {h.backends && (
              <div className="text-gray-500">
                Backends: {Object.entries(h.backends).map(([tier, backend]) => `${tier}→${backend}`).join(', ')}
              </div>
            )}
            {h.models && (
              <div className="text-gray-500 font-mono break-all">
                Models: {Object.entries(h.models).map(([tier, model]) => `${tier}=${model}`).join(', ')}
              </div>
            )}
            {typeof h.fallback_on_quota === 'boolean' && (
              <div className="text-gray-400">Automatic fallback on quota: {h.fallback_on_quota ? 'on' : 'off'}</div>
            )}
            <button onClick={() => { void load() }} className={btnSecondary}>Refresh</button>
          </div>
        )}
      </Field>

      <Field label="LLM Backend" hint="Choose who bills each generation tier. Changes apply immediately and persist in llm-gateway.">
        {loadError && (
          <div className="mb-3 px-3 py-2 rounded-md text-xs bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300">
            Gateway configuration unavailable: {loadError}
          </div>
        )}
        {notice && (
          <div className={`mb-3 px-3 py-2 rounded-md text-xs ${notice.type === 'ok' ? 'bg-green-50 text-green-700 dark:bg-green-900/30 dark:text-green-300' : 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'}`}>
            {notice.msg}
          </div>
        )}
        {!gatewayConfig && (
          <p className="text-xs text-gray-500">{loadError ? 'Controls are disabled until the gateway is reachable.' : 'Loading…'}</p>
        )}
        {gatewayConfig && (
          <div className="space-y-4">
            {tiers.map((tier) => (
              <TierBackendControl
                key={tier.key}
                label={tier.label}
                description={tier.description}
                value={gatewayConfig[tier.key]}
                claudeModel={tier.claudeModel}
                openRouterModel={tier.openRouterModel}
                disabled={saving}
                onChange={(backend) => {
                  void saveUpdate({ [tier.key]: backend }, `${tier.label} tier now uses ${backend === 'claude' ? 'Claude subscription' : 'OpenRouter'}`)
                }}
              />
            ))}

            <div className="rounded-md border border-amber-200 dark:border-amber-800 bg-amber-50/60 dark:bg-amber-950/20 px-3 py-2 text-xs text-amber-900 dark:text-amber-200">
              Cheap OpenRouter calls cost approximately <strong>$0.00215/call</strong> at the measured
              1,669 prompt / 97 completion tokens (2026-08-22), or approximately <strong>$250/month</strong>
              at 117,000 calls. Claude subscription mode has <strong>$0 marginal cost</strong>.
            </div>

            <label className="block max-w-sm text-xs text-gray-500 dark:text-gray-400">
              Max Claude budget (USD per call)
              <div className="mt-1 flex gap-2">
                <input
                  type="number"
                  min="0.01"
                  step="0.01"
                  value={budgetDraft}
                  disabled={saving}
                  onChange={(event) => setBudgetDraft(event.target.value)}
                  className={inputCls}
                />
                <button
                  disabled={saving || !budgetValid || budget === gatewayConfig.MAX_BUDGET_USD}
                  onClick={() => { void saveUpdate({ MAX_BUDGET_USD: budget }, 'Maximum per-call budget saved') }}
                  className={btnPrimary}
                >
                  {saving ? 'Saving…' : 'Save budget'}
                </button>
              </div>
              <span className="mt-1 block text-gray-400 dark:text-gray-500">
                Per-call ceiling passed to the Claude Agent SDK. It is not a daily or monthly spend limit.
              </span>
            </label>
          </div>
        )}
      </Field>
    </>
  )
}

function TierBackendControl({
  label,
  description,
  value,
  claudeModel,
  openRouterModel,
  disabled,
  onChange,
}: {
  label: string
  description: string
  value: 'claude' | 'openrouter'
  claudeModel: string
  openRouterModel: string
  disabled: boolean
  onChange: (value: 'claude' | 'openrouter') => void
}) {
  return (
    <div className="rounded-md border border-gray-200 dark:border-gray-700 p-3">
      <div className="mb-2">
        <div className="text-sm font-medium text-gray-900 dark:text-gray-100">{label}</div>
        <div className="text-xs text-gray-500 dark:text-gray-400">{description}</div>
      </div>
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2" role="group" aria-label={`${label} backend`}>
        {([
          ['claude', 'Claude subscription', claudeModel, '$0 marginal'],
          ['openrouter', 'OpenRouter', openRouterModel, 'metered per token'],
        ] as const).map(([backend, title, model, cost]) => {
          const selected = value === backend
          return (
            <button
              key={backend}
              type="button"
              aria-pressed={selected}
              disabled={disabled}
              onClick={() => onChange(backend)}
              className={`rounded-md border px-3 py-2 text-left transition-colors disabled:opacity-50 ${
                selected
                  ? backend === 'claude'
                    ? 'border-blue-500 bg-blue-50 dark:border-blue-400 dark:bg-blue-950/30'
                    : 'border-amber-500 bg-amber-50 dark:border-amber-400 dark:bg-amber-950/30'
                  : 'border-gray-200 hover:bg-gray-50 dark:border-gray-700 dark:hover:bg-gray-800'
              }`}
            >
              <span className="flex items-center justify-between gap-2 text-xs font-medium text-gray-900 dark:text-gray-100">
                {title}
                {selected && <span className="text-[10px] uppercase tracking-wide">current</span>}
              </span>
              <span className="mt-1 block break-all font-mono text-xs text-gray-500 dark:text-gray-400">{model}</span>
              <span className="mt-1 block text-[11px] text-gray-400 dark:text-gray-500">{cost}</span>
            </button>
          )
        })}
      </div>
    </div>
  )
}

// --- Channel Filters ---

// A row in the rules table. Union of the per-channel keep_regex / drop_regex /
// incident_only maps, edited together and serialized back into those maps on save.
type FilterRule = { id: string; keep: string; drop: string; incident: string }

function rulesFromCfg(cfg: ChannelFilters): FilterRule[] {
  const ids = new Set<string>([
    ...Object.keys(cfg.keep_regex ?? {}),
    ...Object.keys(cfg.drop_regex ?? {}),
    ...Object.keys(cfg.incident_only ?? {}),
  ])
  return Array.from(ids).map((id) => ({
    id,
    keep: cfg.keep_regex?.[id] ?? '',
    drop: cfg.drop_regex?.[id] ?? '',
    incident: (cfg.incident_only?.[id] ?? []).join(', '),
  }))
}

function cfgFromState(ignore: string[], rules: FilterRule[]): ChannelFilters {
  const keep_regex: Record<string, string> = {}
  const drop_regex: Record<string, string> = {}
  const incident_only: Record<string, string[]> = {}
  for (const r of rules) {
    const id = r.id.trim()
    if (!id) continue
    if (r.keep.trim()) keep_regex[id] = r.keep.trim()
    if (r.drop.trim()) drop_regex[id] = r.drop.trim()
    const authors = r.incident.split(',').map((a) => a.trim()).filter(Boolean)
    if (authors.length) incident_only[id] = authors
  }
  return { ignore, keep_regex, drop_regex, incident_only }
}

// ChannelFiltersSection edits settings key graph.channel_filters via its own
// GET/PUT endpoint (separate from the config-struct settings above). Muted/filtered
// messages never reach the LLM extractor — a cost lever, not just noise control.
function ChannelFiltersSection() {
  const [ignore, setIgnore] = useState<string[]>([])
  const [rules, setRules] = useState<FilterRule[]>([])
  const [channels, setChannels] = useState<ChannelCount[]>([])
  const [loaded, setLoaded] = useState(false)
  const [saving, setSaving] = useState(false)
  const [toast, setToast] = useState<{ type: 'ok' | 'err'; msg: string } | null>(null)
  const [pick, setPick] = useState('')

  useEffect(() => {
    Promise.all([fetchChannelFilters(), fetchChannels()])
      .then(([cfg, ch]) => {
        setIgnore(cfg.ignore ?? [])
        setRules(rulesFromCfg(cfg))
        setChannels(ch || [])
        setLoaded(true)
      })
      .catch(() => setToast({ type: 'err', msg: 'Failed to load channel filters' }))
  }, [])

  // channelId -> display name; falls back to the id when unresolved.
  const nameOf = (id: string) => channels.find((c) => c.channel_id === id)?.name || id
  const labelOf = (c: ChannelCount) => (c.name ? `${c.name} (${c.channel_id})` : c.channel_id)

  const save = async () => {
    setSaving(true)
    setToast(null)
    try {
      await saveChannelFilters(cfgFromState(ignore, rules))
      setToast({ type: 'ok', msg: 'Saved' })
    } catch (error: unknown) {
      setToast({ type: 'err', msg: error instanceof Error ? error.message : 'Save failed' })
    } finally {
      setSaving(false)
    }
  }

  const addIgnore = (id: string) => {
    const v = id.trim()
    if (v && !ignore.includes(v)) setIgnore([...ignore, v])
    setPick('')
  }
  const removeIgnore = (id: string) => setIgnore(ignore.filter((x) => x !== id))

  const setRule = (i: number, patch: Partial<FilterRule>) =>
    setRules(rules.map((r, idx) => (idx === i ? { ...r, ...patch } : r)))
  const addRule = () => setRules([...rules, { id: '', keep: '', drop: '', incident: '' }])
  const removeRule = (i: number) => setRules(rules.filter((_, idx) => idx !== i))

  // Channels not already on the ignore list, for the picker.
  const pickable = channels.filter((c) => !ignore.includes(c.channel_id))

  return (
    <Section title="Channel Filters">
      {toast && (
        <div className={`text-sm px-3 py-1.5 rounded-md ${toast.type === 'ok' ? 'bg-green-50 text-green-700 dark:bg-green-900/30 dark:text-green-300' : 'bg-red-50 text-red-700 dark:bg-red-900/30 dark:text-red-300'}`}>
          {toast.msg}
        </div>
      )}

      {!loaded ? (
        <p className="text-sm text-gray-500">Loading…</p>
      ) : (
        <>
          <Field label="Ignore channels" hint="Drop every message from these Slack channels before the LLM extractor — the strongest, cheapest filter. Use for staging/noise channels the graph should never ingest.">
            <div className="flex flex-wrap gap-1.5 mb-2">
              {ignore.length === 0 && <span className="text-xs text-gray-400">No channels ignored.</span>}
              {ignore.map((id) => (
                <span key={id} className="inline-flex items-center gap-1 px-2 py-1 rounded-md text-xs font-mono bg-blue-50 dark:bg-blue-900/20 text-blue-700 dark:text-blue-300 border border-blue-200 dark:border-blue-800">
                  {nameOf(id)}
                  <button onClick={() => removeIgnore(id)} className="ml-0.5 hover:text-red-500 font-sans font-bold" title="Remove">x</button>
                </span>
              ))}
            </div>
            <div className="flex gap-2">
              <select value={pick} onChange={(e) => addIgnore(e.target.value)} className={selectCls}>
                <option value="">+ add channel…</option>
                {pickable.map((c) => (
                  <option key={c.channel_id} value={c.channel_id}>{labelOf(c)}</option>
                ))}
              </select>
            </div>
          </Field>

          <Field label="Rules (ignore-by-rule)" hint="Per-channel regex filters, evaluated after ignore. keep_regex: keep only bodies that match. drop_regex: drop bodies that match (runs after keep, so keep+drop = 'keep this topic but not its routine successes'). incident_only: comma-separated author display names to keep (e.g. PagerDuty) — all other senders dropped. Leave a field blank to skip that rule.">
            <div className="space-y-2">
              {rules.map((r, i) => (
                <div key={i} className="flex flex-wrap gap-2 items-start">
                  <select value={r.id} onChange={(e) => setRule(i, { id: e.target.value })} className={`${selectCls} min-w-[10rem]`}>
                    <option value="">channel…</option>
                    {r.id && !channels.some((c) => c.channel_id === r.id) && <option value={r.id}>{r.id}</option>}
                    {channels.map((c) => (
                      <option key={c.channel_id} value={c.channel_id}>{labelOf(c)}</option>
                    ))}
                  </select>
                  <input type="text" value={r.keep} onChange={(e) => setRule(i, { keep: e.target.value })} placeholder="keep_regex" className={`${inputCls} font-mono text-xs`} />
                  <input type="text" value={r.drop} onChange={(e) => setRule(i, { drop: e.target.value })} placeholder="drop_regex" className={`${inputCls} font-mono text-xs`} />
                  <input type="text" value={r.incident} onChange={(e) => setRule(i, { incident: e.target.value })} placeholder="incident_only authors" className={`${inputCls} text-xs`} />
                  <button onClick={() => removeRule(i)} className={btnSecondary} title="Remove rule">x</button>
                </div>
              ))}
              <button onClick={addRule} className={btnSecondary}>+ add rule</button>
            </div>
          </Field>

          <button disabled={saving} onClick={save} className={btnPrimary}>
            {saving ? 'Saving…' : 'Save filters'}
          </button>
        </>
      )}
    </Section>
  )
}

// --- Constants ---


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
