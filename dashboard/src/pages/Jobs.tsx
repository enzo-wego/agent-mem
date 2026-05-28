import { useEffect, useRef, useState } from 'react'
import { listJobs, retryJob, deleteJob, type JobRow } from '../api'

const JOB_TYPES = [
  'backfill_slack_channel',
  'backfill_slack_thread',
  'fetch_body',
  'describe_attachment',
  'resolve_identity',
  'index_artifact',
  'refresh_slack_groups',
  'import_bamboohr',
  'recompute_person_distance',
]

const STATUSES = ['all', 'queued', 'running', 'done', 'failed']

function relativeTime(iso: string): string {
  const diffMs = Date.now() - new Date(iso).getTime()
  const diffS = Math.floor(Math.abs(diffMs) / 1000)
  if (diffS < 60) return `${diffS}s ago`
  if (diffS < 3600) return `${Math.floor(diffS / 60)}m ago`
  if (diffS < 86400) return `${Math.floor(diffS / 3600)}h ago`
  return `${Math.floor(diffS / 86400)}d ago`
}

function StatusPill({ status }: { status: string }) {
  const colors: Record<string, string> = {
    queued: 'bg-yellow-100 text-yellow-800 dark:bg-yellow-900/30 dark:text-yellow-300',
    running: 'bg-blue-100 text-blue-800 dark:bg-blue-900/30 dark:text-blue-300',
    done: 'bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-300',
    failed: 'bg-red-100 text-red-800 dark:bg-red-900/30 dark:text-red-300',
  }
  return (
    <span className={`text-xs px-2 py-0.5 rounded-full font-medium ${colors[status] ?? 'bg-gray-100 text-gray-700'}`}>
      {status}
    </span>
  )
}

export function JobsPage({ initialJobId }: { initialJobId?: number }) {
  const [statusFilter, setStatusFilter] = useState('all')
  const [typeFilter, setTypeFilter] = useState('')
  const [jobs, setJobs] = useState<JobRow[]>([])
  const [queueDepth, setQueueDepth] = useState<Record<string, number>>({})
  const [loading, setLoading] = useState(false)
  const [expandedId, setExpandedId] = useState<number | null>(initialJobId ?? null)
  const [actionError, setActionError] = useState('')
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const load = async () => {
    setLoading(true)
    try {
      const res = await listJobs(
        statusFilter === 'all' ? undefined : statusFilter,
        typeFilter || undefined,
        50,
      )
      setJobs(res.jobs ?? [])
      setQueueDepth(res.queue_depth ?? {})
    } catch {
      // ignore
    } finally {
      setLoading(false)
    }
  }

  // Auto-refresh while queued or running jobs exist.
  useEffect(() => {
    load()
  }, [statusFilter, typeFilter])

  useEffect(() => {
    const hasActive = (queueDepth['queued'] ?? 0) > 0 || (queueDepth['running'] ?? 0) > 0
    if (hasActive) {
      intervalRef.current = setInterval(load, 5000)
    } else {
      if (intervalRef.current) clearInterval(intervalRef.current)
    }
    return () => {
      if (intervalRef.current) clearInterval(intervalRef.current)
    }
  }, [queueDepth])

  const handleRetry = async (id: number) => {
    setActionError('')
    try {
      await retryJob(id)
      await load()
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : 'Retry failed')
    }
  }

  const handleDelete = async (id: number) => {
    setActionError('')
    try {
      await deleteJob(id)
      await load()
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : 'Delete failed')
    }
  }

  return (
    <div className="space-y-4">
      {/* Header pills */}
      <div className="flex flex-wrap gap-2 items-center">
        {Object.entries(queueDepth).map(([status, count]) => (
          <span key={status} className="text-xs px-3 py-1 rounded-full bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300 font-medium">
            {status.charAt(0).toUpperCase() + status.slice(1)} {count}
          </span>
        ))}
        <button
          onClick={load}
          disabled={loading}
          className="ml-auto text-xs px-3 py-1 rounded-md bg-gray-100 dark:bg-gray-700 hover:bg-gray-200 dark:hover:bg-gray-600 disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Refresh'}
        </button>
      </div>

      {/* Filters */}
      <div className="flex flex-wrap gap-3 items-center">
        <div>
          <label className="text-xs text-gray-500 mr-1">Status</label>
          <select
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
            className="text-sm px-2 py-1 border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 focus:outline-none focus:ring-2 focus:ring-blue-500"
          >
            {STATUSES.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
        </div>
        <div>
          <label className="text-xs text-gray-500 mr-1">Type</label>
          <select
            value={typeFilter}
            onChange={(e) => setTypeFilter(e.target.value)}
            className="text-sm px-2 py-1 border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 focus:outline-none focus:ring-2 focus:ring-blue-500"
          >
            <option value="">all types</option>
            {JOB_TYPES.map((t) => (
              <option key={t} value={t}>{t}</option>
            ))}
          </select>
        </div>
      </div>

      {actionError && (
        <p className="text-red-500 text-sm">{actionError}</p>
      )}

      {/* Table */}
      <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-gray-200 dark:border-gray-700 bg-gray-50 dark:bg-gray-800/50">
              <th className="text-left px-4 py-2.5 text-gray-500 font-normal">ID</th>
              <th className="text-left px-4 py-2.5 text-gray-500 font-normal">Type</th>
              <th className="text-left px-4 py-2.5 text-gray-500 font-normal">Status</th>
              <th className="text-left px-4 py-2.5 text-gray-500 font-normal hidden md:table-cell">Pri</th>
              <th className="text-left px-4 py-2.5 text-gray-500 font-normal hidden md:table-cell">Attempts</th>
              <th className="text-left px-4 py-2.5 text-gray-500 font-normal hidden lg:table-cell">Available</th>
              <th className="text-left px-4 py-2.5 text-gray-500 font-normal hidden lg:table-cell">Last error</th>
              <th className="px-4 py-2.5" />
            </tr>
          </thead>
          <tbody>
            {jobs.length === 0 && (
              <tr>
                <td colSpan={8} className="px-4 py-6 text-center text-gray-400 text-sm">
                  No jobs found.
                </td>
              </tr>
            )}
            {jobs.map((job) => (
              <>
                <tr
                  key={job.id}
                  onClick={() => setExpandedId(expandedId === job.id ? null : job.id)}
                  className="border-b border-gray-100 dark:border-gray-700/50 hover:bg-gray-50 dark:hover:bg-gray-700/30 cursor-pointer"
                >
                  <td className="px-4 py-2.5 font-mono text-xs text-gray-500">{job.id}</td>
                  <td className="px-4 py-2.5 font-mono text-xs">{job.type}</td>
                  <td className="px-4 py-2.5"><StatusPill status={job.status} /></td>
                  <td className="px-4 py-2.5 hidden md:table-cell text-gray-500">{job.priority}</td>
                  <td className="px-4 py-2.5 hidden md:table-cell text-gray-500">{job.attempts}</td>
                  <td className="px-4 py-2.5 hidden lg:table-cell text-gray-500 text-xs">{relativeTime(job.available_at)}</td>
                  <td className="px-4 py-2.5 hidden lg:table-cell text-gray-500 text-xs max-w-[200px] truncate">
                    {job.last_error || '—'}
                  </td>
                  <td className="px-4 py-2.5 text-right space-x-2" onClick={(e) => e.stopPropagation()}>
                    {job.status === 'failed' && (
                      <button
                        onClick={() => handleRetry(job.id)}
                        className="text-xs px-2 py-1 rounded bg-blue-50 dark:bg-blue-900/30 text-blue-700 dark:text-blue-300 hover:bg-blue-100"
                      >
                        Retry
                      </button>
                    )}
                    {job.status === 'queued' && (
                      <button
                        onClick={() => handleDelete(job.id)}
                        className="text-xs px-2 py-1 rounded bg-red-50 dark:bg-red-900/30 text-red-700 dark:text-red-300 hover:bg-red-100"
                      >
                        Delete
                      </button>
                    )}
                  </td>
                </tr>
                {expandedId === job.id && (
                  <tr key={`${job.id}-expanded`} className="border-b border-gray-100 dark:border-gray-700/50 bg-gray-50 dark:bg-gray-800/50">
                    <td colSpan={8} className="px-4 py-3">
                      <pre className="text-xs font-mono text-gray-700 dark:text-gray-300 whitespace-pre-wrap break-all">
                        {JSON.stringify(job.payload, null, 2)}
                      </pre>
                      {job.last_error && (
                        <p className="text-xs text-red-500 mt-2 font-mono">{job.last_error}</p>
                      )}
                    </td>
                  </tr>
                )}
              </>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
