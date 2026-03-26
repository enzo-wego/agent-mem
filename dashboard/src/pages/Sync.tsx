import { useEffect, useState } from 'react'
import { fetchSyncInfo, fetchHealth, fetchCloudStats, type SyncInfo, type HealthResponse, type StatsResponse } from '../api'

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
      .catch(() => {}) // sync may not be configured

    // Fetch cloud stats via local proxy
    fetchCloudStats().then(setCloudStats).catch(() => {})
  }, [])

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

      {/* Sync Status */}
      <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
        <h3 className="font-semibold mb-3">Sync Status</h3>
        {syncInfo ? (
          <>
            <div className="grid grid-cols-2 gap-4 text-sm mb-4">
              <div>
                <span className="text-gray-500">Mode</span>
                <p className="font-medium">{syncInfo.mode}</p>
              </div>
              <div>
                <span className="text-gray-500">Machine ID</span>
                <p className="font-medium font-mono text-xs">{syncInfo.machine_id || 'N/A'}</p>
              </div>
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
            </div>

            {syncInfo.stats && syncInfo.stats.length > 0 && (
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-gray-200 dark:border-gray-700">
                    <th className="text-left py-2 text-gray-500 font-normal">Table</th>
                    <th className="text-right py-2 text-gray-500 font-normal">Total</th>
                    <th className="text-right py-2 text-gray-500 font-normal">Unsynced</th>
                  </tr>
                </thead>
                <tbody>
                  {syncInfo.stats.map((s) => (
                    <tr key={s.table} className="border-b border-gray-100 dark:border-gray-700/50">
                      <td className="py-2">{s.table}</td>
                      <td className="text-right py-2">{s.total.toLocaleString()}</td>
                      <td className="text-right py-2">{s.unsynced}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </>
        ) : (
          <p className="text-gray-500 text-sm">Sync not configured or unavailable.</p>
        )}
      </div>

      {/* Cloud Stats */}
      {cloudStats && (
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
