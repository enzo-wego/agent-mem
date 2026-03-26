import { useEffect, useState } from 'react'
import { listSummaries, type Summary } from '../api'

function Section({ label, text }: { label: string; text?: string }) {
  if (!text) return null
  return (
    <div className="mt-3">
      <h5 className="text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400 mb-1">{label}</h5>
      <p className="text-sm text-gray-700 dark:text-gray-300 whitespace-pre-wrap">{text}</p>
    </div>
  )
}

export function SessionsPage({ project }: { project: string }) {
  const [summaries, setSummaries] = useState<Summary[]>([])
  const [expanded, setExpanded] = useState<Set<number>>(new Set())
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (!project) return
    setLoading(true)
    listSummaries(project, 50)
      .then((data) => setSummaries(data.summaries || []))
      .finally(() => setLoading(false))
  }, [project])

  const toggle = (id: number) => {
    setExpanded((prev) => {
      const next = new Set(prev)
      next.has(id) ? next.delete(id) : next.add(id)
      return next
    })
  }

  if (!project) {
    return <p className="text-gray-500">Select a project to view sessions.</p>
  }

  return (
    <div>
      {loading && <p className="text-gray-500">Loading...</p>}

      <div className="space-y-3">
        {summaries.map((s) => (
          <div
            key={s.id}
            className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4 cursor-pointer hover:border-indigo-300 dark:hover:border-indigo-600 transition-colors"
            onClick={() => toggle(s.id)}
          >
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-2">
                <span className="text-xs px-2 py-0.5 rounded-full bg-indigo-100 text-indigo-700 dark:bg-indigo-900 dark:text-indigo-300">
                  session summary
                </span>
                <span className="text-xs text-gray-400">
                  {new Date(s.created_at).toLocaleDateString('en-US', {
                    month: 'short', day: 'numeric',
                  })}{' '}
                  {new Date(s.created_at).toLocaleTimeString('en-US', {
                    hour: 'numeric', minute: '2-digit', second: '2-digit',
                  })}
                </span>
              </div>
            </div>
            <h4 className="font-medium text-sm mt-1">{s.request || 'Session Summary'}</h4>

            {expanded.has(s.id) && (
              <div className="mt-2 pl-4 border-l-2 border-indigo-200 dark:border-indigo-700">
                <Section label="Investigated" text={s.investigated} />
                <Section label="Learned" text={s.learned} />
                <Section label="Completed" text={s.completed} />
                <Section label="Next Steps" text={s.next_steps} />
                {s.notes && <Section label="Notes" text={s.notes} />}
              </div>
            )}
          </div>
        ))}
      </div>

      {!loading && summaries.length === 0 && (
        <p className="text-gray-500">No session summaries found.</p>
      )}
    </div>
  )
}
