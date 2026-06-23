import { useState } from 'react'
import { backfillSlack, type BackfillSlackResponse } from '../api'

export function BackfillPage({ onNavigateJobs }: { onNavigateJobs?: (jobId?: number) => void }) {
  const [channelId, setChannelId] = useState('')
  const [months, setMonths] = useState(3)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<BackfillSlackResponse | null>(null)

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setResult(null)

    if (!channelId.trim()) {
      setError('Channel ID is required.')
      return
    }
    if (!/^C[A-Z0-9]+$/.test(channelId.trim())) {
      setError('Channel ID must match C[A-Z0-9]+ (e.g. C08S954G2LX).')
      return
    }
    if (months < 1 || months > 24) {
      setError('Months must be between 1 and 24.')
      return
    }

    setLoading(true)
    try {
      const res = await backfillSlack(channelId.trim(), months)
      setResult(res)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Request failed')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="max-w-lg">
      <h2 className="text-lg font-semibold mb-1">Slack Channel Backfill</h2>
      <p className="text-sm text-gray-500 dark:text-gray-400 mb-6">
        Enqueues a job to ingest historical messages from a Slack channel into the graph.
      </p>

      <form onSubmit={handleSubmit} className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-6 space-y-4">
        <div>
          <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
            Channel ID
          </label>
          <input
            type="text"
            value={channelId}
            onChange={(e) => setChannelId(e.target.value)}
            placeholder="C08S954G2LX"
            className="w-full px-3 py-2 border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 text-gray-900 dark:text-gray-100 focus:outline-none focus:ring-2 focus:ring-blue-500 font-mono text-sm"
          />
          <p className="text-xs text-gray-400 mt-1">Find this in the channel's About section in Slack.</p>
        </div>

        <div>
          <label className="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
            Look back (months)
          </label>
          <input
            type="number"
            min={1}
            max={24}
            value={months}
            onChange={(e) => setMonths(Number(e.target.value))}
            className="w-24 px-3 py-2 border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 text-gray-900 dark:text-gray-100 focus:outline-none focus:ring-2 focus:ring-blue-500"
          />
          <span className="text-xs text-gray-400 ml-2">1 – 24 months</span>
        </div>

        {error && (
          <p className="text-red-500 text-sm">{error}</p>
        )}

        <button
          type="submit"
          disabled={loading}
          className="w-full py-2 bg-blue-600 text-white rounded-md hover:bg-blue-700 disabled:opacity-50 transition-colors font-medium"
        >
          {loading ? 'Enqueueing...' : 'Start backfill'}
        </button>
      </form>

      {result && (
        <div className="mt-6 bg-green-50 dark:bg-green-900/20 border border-green-200 dark:border-green-800 rounded-lg p-4">
          <p className="text-sm font-medium text-green-800 dark:text-green-300 mb-2">
            Backfill job queued successfully
          </p>
          <dl className="text-sm space-y-1 text-gray-700 dark:text-gray-300">
            <div className="flex gap-2">
              <dt className="text-gray-500 w-32 shrink-0">Job ID</dt>
              <dd className="font-mono">{result.job_id}</dd>
            </div>
            <div className="flex gap-2">
              <dt className="text-gray-500 w-32 shrink-0">Channel</dt>
              <dd className="font-mono">{result.channel_id}</dd>
            </div>
            <div className="flex gap-2">
              <dt className="text-gray-500 w-32 shrink-0">From</dt>
              <dd>{result.estimated_months} month(s) ago</dd>
            </div>
            <div className="flex gap-2">
              <dt className="text-gray-500 w-32 shrink-0">Oldest TS</dt>
              <dd className="font-mono text-xs">{result.oldest_ts}</dd>
            </div>
          </dl>
          {onNavigateJobs && (
            <button
              onClick={() => onNavigateJobs(result.job_id)}
              className="mt-3 text-sm text-blue-600 dark:text-blue-400 hover:underline"
            >
              View job in Jobs inspector
            </button>
          )}
        </div>
      )}
    </div>
  )
}
