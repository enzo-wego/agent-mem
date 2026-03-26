import { useEffect, useState, useRef } from 'react'
import { fetchLogs, type LogEntry } from '../api'

const LEVELS = ['trace', 'debug', 'info', 'warn', 'error'] as const
const POLL_INTERVAL = 3000

export function LogsPage() {
  const [entries, setEntries] = useState<LogEntry[]>([])
  const [level, setLevel] = useState('info')
  const [autoScroll, setAutoScroll] = useState(true)
  const [error, setError] = useState('')
  const bottomRef = useRef<HTMLDivElement>(null)

  const load = () => {
    fetchLogs(level)
      .then((r) => { setEntries(r.entries || []); setError('') })
      .catch(() => setError('Failed to load logs'))
  }

  useEffect(() => { load() }, [level])

  // Auto-poll
  useEffect(() => {
    const id = setInterval(load, POLL_INTERVAL)
    return () => clearInterval(id)
  }, [level])

  // Auto-scroll to bottom
  useEffect(() => {
    if (autoScroll && bottomRef.current) {
      bottomRef.current.scrollIntoView({ behavior: 'smooth' })
    }
  }, [entries, autoScroll])

  return (
    <div className="space-y-3">
      {/* Controls */}
      <div className="flex items-center gap-4">
        <div className="flex items-center gap-2">
          <label className="text-sm text-gray-500">Level:</label>
          <select
            value={level}
            onChange={(e) => setLevel(e.target.value)}
            className="px-2 py-1 text-sm border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 focus:outline-none focus:ring-2 focus:ring-blue-500"
          >
            {LEVELS.map((l) => (
              <option key={l} value={l}>{l}</option>
            ))}
          </select>
        </div>
        <label className="flex items-center gap-1.5 text-sm text-gray-500 cursor-pointer">
          <input
            type="checkbox"
            checked={autoScroll}
            onChange={(e) => setAutoScroll(e.target.checked)}
            className="rounded border-gray-300 text-blue-600"
          />
          Auto-scroll
        </label>
        <span className="text-xs text-gray-400">{entries.length} entries (polling every 3s)</span>
        <button
          onClick={load}
          className="ml-auto px-2 py-1 text-xs font-medium rounded-md border border-gray-300 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-700"
        >
          Refresh
        </button>
      </div>

      {error && <p className="text-red-500 text-sm">{error}</p>}

      {/* Log output */}
      <div className="bg-gray-950 rounded-lg border border-gray-800 p-3 h-[calc(100vh-240px)] overflow-y-auto font-mono text-xs leading-relaxed">
        {entries.length === 0 && (
          <p className="text-gray-500">No log entries.</p>
        )}
        {entries.map((e, i) => (
          <div key={i} className="flex gap-2 hover:bg-gray-900/50 py-px">
            <span className="text-gray-600 shrink-0 w-20">{formatTime(e.time)}</span>
            <span className={`shrink-0 w-12 font-semibold ${levelColor(e.level)}`}>
              {e.level.toUpperCase()}
            </span>
            <span className="text-gray-300 whitespace-pre-wrap break-all">{e.message}</span>
          </div>
        ))}
        <div ref={bottomRef} />
      </div>
    </div>
  )
}

function formatTime(iso: string): string {
  try {
    const d = new Date(iso)
    return d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
  } catch {
    return ''
  }
}

function levelColor(level: string): string {
  switch (level) {
    case 'trace': return 'text-gray-500'
    case 'debug': return 'text-gray-400'
    case 'info': return 'text-green-400'
    case 'warn': return 'text-yellow-400'
    case 'error': return 'text-red-400'
    case 'fatal': return 'text-red-600'
    default: return 'text-gray-400'
  }
}
