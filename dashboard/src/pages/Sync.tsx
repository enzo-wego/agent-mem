import { useEffect, useState } from 'react'
import {
  fetchSyncInfo,
  fetchHealth,
  fetchCloudStats,
  fetchJiraUpdates,
  fetchGHKeySearch,
  type SyncInfo,
  type HealthResponse,
  type StatsResponse,
  type JiraUpdatesStatus,
  type GHKeySearchStatus,
} from '../api'
import { jiraSyncState, type JiraSyncState } from '../jiraSyncState'

export function SyncPage() {
  const [syncInfo, setSyncInfo] = useState<SyncInfo | null>(null)
  const [health, setHealth] = useState<HealthResponse | null>(null)
  const [cloudStats, setCloudStats] = useState<StatsResponse | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    fetchHealth()
      .then(setHealth)
      .catch(() => setError('Worker unreachable'))

    fetchSyncInfo()
      .then(setSyncInfo)
      .catch(() => {})

    fetchCloudStats().then(setCloudStats).catch(() => {})
  }, [])

  const isCloud = syncInfo?.mode === 'cloud'

  return (
    <div className="space-y-6">
      {/* Health */}
      <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
        <h3 className="font-semibold mb-3">Worker Health</h3>
        {error && <p className="text-red-500 text-sm">{error}</p>}
        {health && (
          <div className="grid grid-cols-3 gap-4 text-sm">
            <div>
              <span className="text-gray-500">Status</span>
              <p className="font-medium">{health.status}</p>
            </div>
            <div>
              <span className="text-gray-500">PostgreSQL</span>
              <p className="font-medium">{health.postgres ? 'Connected' : 'Disconnected'}</p>
            </div>
            <div>
              <span className="text-gray-500">Pending Messages</span>
              <p className="font-medium">{health.pending_messages}</p>
            </div>
          </div>
        )}
      </div>

      <JiraFreshnessRow />
      <GHKeySearchRow />

      {/* Sync Status */}
      <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
        <h3 className="font-semibold mb-3">Sync Status</h3>
        {syncInfo ? (
          <>
            <div className="grid grid-cols-2 gap-4 text-sm mb-4">
              <div>
                <span className="text-gray-500">Mode</span>
                <p className="font-medium">{isCloud ? 'Cloud (receive-only)' : 'Local'}</p>
              </div>
              <div>
                <span className="text-gray-500">Machine ID</span>
                <p className="font-medium font-mono text-xs">{syncInfo.machine_id || 'N/A'}</p>
              </div>
              {!isCloud && (
                <>
                  <div>
                    <span className="text-gray-500">Sync</span>
                    <p className="font-medium">
                      {syncInfo.sync_enabled ? `Enabled (every ${syncInfo.sync_interval})` : 'Disabled'}
                    </p>
                  </div>
                  <div>
                    <span className="text-gray-500">Last Push</span>
                    <p className="font-medium">
                      {syncInfo.last_push ? new Date(syncInfo.last_push).toLocaleString() : 'Never'}
                    </p>
                  </div>
                  <div>
                    <span className="text-gray-500">Last Pull</span>
                    <p className="font-medium">
                      {syncInfo.last_pull ? new Date(syncInfo.last_pull).toLocaleString() : 'Never'}
                    </p>
                  </div>
                </>
              )}
            </div>

            {/* Data table */}
            {syncInfo.stats && syncInfo.stats.length > 0 && (
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-gray-200 dark:border-gray-700">
                    <th className="text-left py-2 text-gray-500 font-normal">Table</th>
                    <th className="text-right py-2 text-gray-500 font-normal">Total</th>
                    {!isCloud && (
                      <th className="text-right py-2 text-gray-500 font-normal">Unsynced</th>
                    )}
                  </tr>
                </thead>
                <tbody>
                  {/* Group rows by the `graph.` name prefix — Flat Memory vs
                      Graph Memory. The prefix is the grouping key; no `kind`
                      field is added to SyncStats / api.ts. Backend order is
                      preserved within each group. */}
                  {(() => {
                    const colSpan = isCloud ? 2 : 3
                    const flat = syncInfo.stats.filter((s) => !s.table.startsWith('graph.'))
                    const graph = syncInfo.stats.filter((s) => s.table.startsWith('graph.'))
                    const renderRow = (s: SyncInfo['stats'][number]) => (
                      <tr key={s.table} className="border-b border-gray-100 dark:border-gray-700/50">
                        <td className="py-2 pl-4">{s.table}</td>
                        <td className="text-right py-2">{s.total.toLocaleString()}</td>
                        {!isCloud && (
                          <td className="text-right py-2">{s.unsynced}</td>
                        )}
                      </tr>
                    )
                    const heading = (label: string) => (
                      <tr className="border-b border-gray-200 dark:border-gray-700">
                        <td colSpan={colSpan} className="py-2 font-semibold text-gray-600 dark:text-gray-300">
                          {label}
                        </td>
                      </tr>
                    )
                    return (
                      <>
                        {flat.length > 0 && (
                          <>
                            {heading('Flat Memory')}
                            {flat.map(renderRow)}
                          </>
                        )}
                        {graph.length > 0 && (
                          <>
                            {heading('Graph Memory')}
                            {graph.map(renderRow)}
                          </>
                        )}
                      </>
                    )
                  })()}
                </tbody>
              </table>
            )}
          </>
        ) : (
          <p className="text-gray-500 text-sm">Sync not configured or unavailable.</p>
        )}
      </div>

      {/* Connected Clients (cloud mode) */}
      {isCloud && syncInfo?.clients && syncInfo.clients.length > 0 && (
        <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
          <h3 className="font-semibold mb-3">Connected Clients</h3>
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-gray-200 dark:border-gray-700">
                <th className="text-left py-2 text-gray-500 font-normal">Machine ID</th>
                <th className="text-right py-2 text-gray-500 font-normal">Last Push</th>
                <th className="text-right py-2 text-gray-500 font-normal">Last Pull</th>
              </tr>
            </thead>
            <tbody>
              {syncInfo.clients.map((c) => (
                <tr key={c.machine_id} className="border-b border-gray-100 dark:border-gray-700/50">
                  <td className="py-2 font-mono text-xs">{c.machine_id}</td>
                  <td className="text-right py-2">
                    {c.last_push ? new Date(c.last_push).toLocaleString() : 'Never'}
                  </td>
                  <td className="text-right py-2">
                    {c.last_pull ? new Date(c.last_pull).toLocaleString() : 'Never'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Cloud Stats (only in local mode) */}
      {cloudStats && !isCloud && (
        <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
          <h3 className="font-semibold mb-3">Cloud Statistics</h3>
          <div className="grid grid-cols-3 gap-4 text-sm">
            <div>
              <span className="text-gray-500">Observations</span>
              <p className="font-medium">{cloudStats.observations.toLocaleString()}</p>
            </div>
            <div>
              <span className="text-gray-500">Summaries</span>
              <p className="font-medium">{cloudStats.summaries.toLocaleString()}</p>
            </div>
            <div>
              <span className="text-gray-500">Prompts</span>
              <p className="font-medium">{cloudStats.prompts.toLocaleString()}</p>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

const JIRA_STATE_STYLE: Record<JiraSyncState, { dot: string; label: string }> = {
  unavailable: { dot: 'bg-gray-400', label: 'Unavailable' },
  off: { dot: 'bg-gray-400', label: 'Off' },
  never: { dot: 'bg-red-500', label: 'Never succeeded' },
  stale: { dot: 'bg-red-500', label: 'Stale' },
  error: { dot: 'bg-amber-500', label: 'Last run failed' },
  ok: { dot: 'bg-green-500', label: 'OK' },
}

function relativeTime(iso: string, now: Date): string {
  const s = Math.max(0, Math.round((now.getTime() - new Date(iso).getTime()) / 1000))
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

// Jira freshness: health of the refresh_jira_updates poll. Refetched every 60 s.
function JiraFreshnessRow() {
  const [cfg, setCfg] = useState<JiraUpdatesStatus | null>(null)
  const [now, setNow] = useState(() => new Date())

  useEffect(() => {
    const load = () =>
      fetchJiraUpdates()
        .then(setCfg)
        .catch(() => setCfg(null))
        .finally(() => setNow(new Date()))
    load()
    const id = setInterval(load, 60_000)
    return () => clearInterval(id)
  }, [])

  const state = jiraSyncState(cfg, now)
  const style = JIRA_STATE_STYLE[state]
  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
      <div className="flex items-center justify-between text-sm">
        <div className="flex items-center gap-2">
          <span className={`inline-block w-2.5 h-2.5 rounded-full ${style.dot}`} />
          <span className="font-semibold">Jira freshness</span>
          <span className="text-gray-500">{style.label}</span>
        </div>
        {cfg && (
          <div className="flex items-center gap-4 text-gray-500">
            <span>Last OK: {cfg.last_ok_at ? relativeTime(cfg.last_ok_at, now) : 'never'}</span>
            <span>{cfg.last_queued ?? 0} queued</span>
          </div>
        )}
      </div>
      {state === 'error' && cfg?.last_error && (
        <p className="mt-2 text-xs text-amber-700 dark:text-amber-300 font-mono break-all">{cfg.last_error}</p>
      )}
    </div>
  )
}

type GHKeyState = 'disabled' | 'never' | 'error' | 'ok'

const GH_KEY_STATE_STYLE: Record<GHKeyState, { dot: string; label: string }> = {
  disabled: { dot: 'bg-gray-400', label: 'disabled' },
  never: { dot: 'bg-amber-500', label: 'never succeeded' },
  error: { dot: 'bg-amber-500', label: 'error' },
  ok: { dot: 'bg-green-500', label: 'ok' },
}

function ghKeyState(cfg: GHKeySearchStatus | null): GHKeyState {
  if (!cfg || !cfg.enabled) return 'disabled'
  if (cfg.last_error) return 'error'
  return cfg.last_ok_at ? 'ok' : 'never'
}

// GitHub PR key search: health of the refresh_gh_key_search poll. Refetched every 60 s.
function GHKeySearchRow() {
  const [cfg, setCfg] = useState<GHKeySearchStatus | null>(null)
  const [now, setNow] = useState(() => new Date())

  useEffect(() => {
    const load = () =>
      fetchGHKeySearch()
        .then(setCfg)
        .catch(() => setCfg(null))
        .finally(() => setNow(new Date()))
    load()
    const id = setInterval(load, 60_000)
    return () => clearInterval(id)
  }, [])

  const state = ghKeyState(cfg)
  const style = GH_KEY_STATE_STYLE[state]
  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
      <div className="flex items-center justify-between text-sm">
        <div className="flex items-center gap-2">
          <span className={`inline-block w-2.5 h-2.5 rounded-full ${style.dot}`} />
          <span className="font-semibold">GitHub PR key search</span>
          <span className="text-gray-500">{style.label}</span>
        </div>
        {cfg && (
          <div className="flex items-center gap-4 text-gray-500">
            <span>Last run: {cfg.last_run_at ? relativeTime(cfg.last_run_at, now) : 'never'}</span>
            <span>{cfg.last_linked ?? 0} linked</span>
            <span>{cfg.last_unmatched ?? 0} unmatched</span>
            <span>Cursor: {cfg.cursor ?? 'none'}</span>
          </div>
        )}
      </div>
      {cfg?.last_error && (
        <p className="mt-2 text-xs text-amber-700 dark:text-amber-300 font-mono break-all">{cfg.last_error}</p>
      )}
    </div>
  )
}
