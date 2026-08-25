import { useEffect, useState } from 'react'
import {
  fetchChannels,
  fetchContinents,
  saveContinents,
  type ChannelCount,
  type Continent,
  type ContinentCfg,
} from '../api'
import { continentOf, nameOf } from '../continents'

export function ContinentsPage() {
  const [cfg, setCfg] = useState<ContinentCfg | null>(null)
  const [channels, setChannels] = useState<ChannelCount[]>([])
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)
  const [saving, setSaving] = useState(false)

  const load = () => {
    setError('')
    Promise.all([fetchChannels(), fetchContinents()])
      .then(([ch, c]) => {
        setChannels(ch || [])
        setCfg(c)
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : 'Failed to load'))
  }

  useEffect(load, [])

  if (!cfg) {
    return (
      <div>
        <h2 className="text-lg font-semibold mb-1">Continents</h2>
        {error ? <p className="text-red-500 text-sm">{error}</p> : <p className="text-sm text-gray-500">Loading…</p>}
      </div>
    )
  }

  const update = (next: ContinentCfg) => {
    setCfg({ ...next })
    setSaved(false)
  }

  const updateContinent = (idx: number, patch: Partial<Continent>) => {
    const continents = cfg.continents.map((c, i) => (i === idx ? { ...c, ...patch } : c))
    update({ ...cfg, continents })
  }

  const addContinent = () => {
    const continents = [
      ...cfg.continents,
      { id: `c${Date.now()}`, label: 'New continent', color: '#58a6ff', center: [0, 0] as [number, number], match: [] },
    ]
    update({ ...cfg, continents })
  }

  const removeContinent = (idx: number) => {
    const continents = cfg.continents.filter((_, i) => i !== idx)
    update({ ...cfg, continents })
  }

  const setName = (channelId: string, name: string) => {
    const names = { ...cfg.names }
    if (name.trim()) names[channelId] = name
    else delete names[channelId]
    update({ ...cfg, names })
  }

  const setOverride = (channelId: string, continentId: string) => {
    const overrides = { ...cfg.overrides }
    if (continentId) overrides[channelId] = continentId
    else delete overrides[channelId]
    update({ ...cfg, overrides })
  }
  const toggleNotify = (channelId: string, notify: boolean) => {
    const cur = cfg.ignore ?? []
    const ignore = notify
      ? cur.filter((id) => id !== channelId)
      : cur.includes(channelId)
        ? cur
        : [...cur, channelId]
    update({ ...cfg, ignore })
  }

  const handleSave = async () => {
    setSaving(true)
    setError('')
    setSaved(false)
    try {
      const next = await saveContinents(cfg)
      setCfg(next)
      setSaved(true)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  const inputCls =
    'px-2 py-1 border border-gray-300 dark:border-gray-600 rounded bg-white dark:bg-gray-700 text-gray-900 dark:text-gray-100 focus:outline-none focus:ring-2 focus:ring-blue-500 text-sm'

  return (
    <div>
      <div className="flex items-center justify-between mb-1">
        <h2 className="text-lg font-semibold">Continents</h2>
        <button
          onClick={handleSave}
          disabled={saving}
          className="px-4 py-1.5 bg-blue-600 text-white rounded-md hover:bg-blue-700 disabled:opacity-50 transition-colors text-sm font-medium"
        >
          {saving ? 'Saving…' : 'Save'}
        </button>
      </div>
      <p className="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Configure how Slack channels group into continents on the Globe. The Notify column controls DM
        notifications — unticking mutes a channel&apos;s alerts without stopping ingestion (use the
        Settings page&apos;s channel filters to drop a channel entirely).
      </p>

      {error && <p className="text-red-500 text-sm mb-3">{error}</p>}
      {saved && <p className="text-green-600 dark:text-green-400 text-sm mb-3">Saved.</p>}

      {/* Continent definitions */}
      <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4 mb-6">
        <h3 className="text-sm font-semibold mb-3">Continents</h3>
        <div className="space-y-3">
          {cfg.continents.map((c, idx) => (
            <div key={idx} className="flex flex-wrap items-center gap-2">
              <input
                type="color"
                value={c.color}
                onChange={(e) => updateContinent(idx, { color: e.target.value })}
                className="w-9 h-9 rounded border border-gray-300 dark:border-gray-600 bg-transparent cursor-pointer"
                title="Color"
              />
              <input
                type="text"
                value={c.label}
                onChange={(e) => updateContinent(idx, { label: e.target.value })}
                placeholder="Label"
                className={`${inputCls} w-40`}
              />
              <input
                type="number"
                value={c.center[0]}
                onChange={(e) => updateContinent(idx, { center: [Number(e.target.value), c.center[1]] })}
                placeholder="lat"
                className={`${inputCls} w-20`}
                title="Center latitude"
              />
              <input
                type="number"
                value={c.center[1]}
                onChange={(e) => updateContinent(idx, { center: [c.center[0], Number(e.target.value)] })}
                placeholder="lon"
                className={`${inputCls} w-20`}
                title="Center longitude"
              />
              <input
                type="text"
                value={c.match.join(', ')}
                onChange={(e) =>
                  updateContinent(idx, {
                    match: e.target.value.split(',').map((s) => s.trim()).filter(Boolean),
                  })
                }
                placeholder="prefixes (comma-separated, * = catch-all)"
                className={`${inputCls} flex-1 min-w-[16rem] font-mono`}
                title="Match prefixes"
              />
              <button
                onClick={() => removeContinent(idx)}
                className="px-2 py-1 text-sm text-red-600 dark:text-red-400 hover:underline"
              >
                Remove
              </button>
            </div>
          ))}
        </div>
        <button onClick={addContinent} className="mt-3 text-sm text-blue-600 dark:text-blue-400 hover:underline">
          + Add continent
        </button>
      </div>

      {/* Channels table */}
      <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 overflow-hidden">
        <h3 className="text-sm font-semibold p-4 pb-2">Channels ({channels.length})</h3>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-gray-500 dark:text-gray-400 border-b border-gray-200 dark:border-gray-700">
                <th className="px-4 py-2 font-medium">Channel ID</th>
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium text-right">Msgs</th>
                <th className="px-4 py-2 font-medium">Continent</th>
                <th className="px-4 py-2 font-medium">Notify</th>
              </tr>
            </thead>
            <tbody>
              {channels.map((ch) => {
                const auto = continentOf(ch.channel_id, { ...cfg, overrides: {} })
                const autoLabel = cfg.continents.find((c) => c.id === auto)?.label || 'none'
                return (
                  <tr key={ch.channel_id} className="border-b border-gray-100 dark:border-gray-700/50">
                    <td className="px-4 py-2 font-mono text-xs text-gray-500 dark:text-gray-400">{ch.channel_id}</td>
                    <td className="px-4 py-2">
                      <input
                        type="text"
                        value={cfg.names[ch.channel_id] || ''}
                        onChange={(e) => setName(ch.channel_id, e.target.value)}
                        placeholder={nameOf(ch.channel_id, cfg)}
                        className={`${inputCls} w-56`}
                      />
                    </td>
                    <td className="px-4 py-2 text-right tabular-nums">{ch.count}</td>
                    <td className="px-4 py-2">
                      <select
                        value={cfg.overrides[ch.channel_id] || ''}
                        onChange={(e) => setOverride(ch.channel_id, e.target.value)}
                        className={inputCls}
                      >
                        <option value="">Auto ({autoLabel})</option>
                        {cfg.continents.map((c) => (
                          <option key={c.id} value={c.id}>
                            {c.label}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td className="px-4 py-2 whitespace-nowrap">
                      <label className="inline-flex items-center gap-1.5">
                        <input
                          type="checkbox"
                          checked={!(cfg.ignore ?? []).includes(ch.channel_id)}
                          onChange={(e) => toggleNotify(ch.channel_id, e.target.checked)}
                          title="Send DM notifications for this channel"
                        />
                        {!(cfg.ignore ?? []).includes(ch.channel_id) ? null : (
                          <span className="text-xs text-gray-400 dark:text-gray-500">muted</span>
                        )}
                      </label>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  )
}
