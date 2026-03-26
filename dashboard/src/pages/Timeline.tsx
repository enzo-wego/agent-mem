import { useEffect, useState } from 'react'
import { listObservations, listSummaries, listPrompts, getObservation, type SearchResult, type Summary, type Prompt, type ObservationDetail } from '../api'

const obsTypes = ['', 'decision', 'bugfix', 'feature', 'refactor', 'discovery', 'change']
const typeColors: Record<string, string> = {
  decision: 'bg-purple-100 text-purple-700 dark:bg-purple-900 dark:text-purple-300',
  bugfix: 'bg-red-100 text-red-700 dark:bg-red-900 dark:text-red-300',
  feature: 'bg-green-100 text-green-700 dark:bg-green-900 dark:text-green-300',
  refactor: 'bg-blue-100 text-blue-700 dark:bg-blue-900 dark:text-blue-300',
  discovery: 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900 dark:text-yellow-300',
  change: 'bg-orange-100 text-orange-700 dark:bg-orange-900 dark:text-orange-300',
  summary: 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900 dark:text-indigo-300',
  prompt: 'bg-cyan-100 text-cyan-700 dark:bg-cyan-900 dark:text-cyan-300',
}

type TimelineItem =
  | { kind: 'observation'; data: SearchResult }
  | { kind: 'summary'; data: Summary }
  | { kind: 'prompt'; data: Prompt }

function formatTime(dateStr: string): string {
  return new Date(dateStr).toLocaleTimeString('en-US', {
    hour: 'numeric', minute: '2-digit', second: '2-digit',
  })
}

function formatDate(dateStr: string): string {
  return new Date(dateStr).toLocaleDateString('en-US', {
    year: 'numeric', month: 'short', day: 'numeric',
  })
}

function SummarySection({ label, text }: { label: string; text?: string }) {
  if (!text) return null
  return (
    <div className="mt-2">
      <h5 className="text-xs font-semibold uppercase text-gray-500 dark:text-gray-400 mb-0.5">{label}</h5>
      <p className="text-sm text-gray-700 dark:text-gray-300 whitespace-pre-wrap">{text}</p>
    </div>
  )
}

function truncatePrompt(text: string, max = 200): string {
  const cleaned = text.replace(/<[^>]+>/g, '').replace(/\s+/g, ' ').trim()
  if (cleaned.length <= max) return cleaned
  return cleaned.slice(0, max) + '...'
}

function ObsDetail({ detail }: { detail: ObservationDetail }) {
  const [view, setView] = useState<'subtitle' | 'facts' | 'narrative'>('subtitle')
  return (
    <div className="mt-2 pl-4 border-l-2 border-gray-200 dark:border-gray-600">
      <div className="flex gap-2 mb-2">
        {detail.subtitle && (
          <button onClick={(e) => { e.stopPropagation(); setView('subtitle') }}
            className={`text-xs px-2 py-0.5 rounded ${view === 'subtitle' ? 'bg-gray-200 dark:bg-gray-600' : 'text-gray-400 hover:text-gray-600'}`}>
            subtitle
          </button>
        )}
        {detail.facts?.length > 0 && (
          <button onClick={(e) => { e.stopPropagation(); setView('facts') }}
            className={`text-xs px-2 py-0.5 rounded ${view === 'facts' ? 'bg-gray-200 dark:bg-gray-600' : 'text-gray-400 hover:text-gray-600'}`}>
            facts ({detail.facts.length})
          </button>
        )}
        {detail.narrative && (
          <button onClick={(e) => { e.stopPropagation(); setView('narrative') }}
            className={`text-xs px-2 py-0.5 rounded ${view === 'narrative' ? 'bg-gray-200 dark:bg-gray-600' : 'text-gray-400 hover:text-gray-600'}`}>
            narrative
          </button>
        )}
      </div>

      {view === 'subtitle' && detail.subtitle && (
        <p className="text-sm text-gray-600 dark:text-gray-400">{detail.subtitle}</p>
      )}

      {view === 'facts' && detail.facts && (
        <ul className="text-sm text-gray-600 dark:text-gray-400 space-y-1">
          {detail.facts.map((f, i) => (
            <li key={i} className="flex gap-1.5">
              <span className="text-gray-400 select-none">•</span>
              <span>{f}</span>
            </li>
          ))}
        </ul>
      )}

      {view === 'narrative' && detail.narrative && (
        <div className="text-sm text-gray-600 dark:text-gray-400 max-h-72 overflow-y-auto whitespace-pre-wrap">
          {detail.narrative}
        </div>
      )}

      {/* Concepts */}
      {detail.concepts?.length > 0 && (
        <div className="mt-2 flex gap-1 flex-wrap">
          {detail.concepts.map((c, i) => (
            <span key={i} className="text-[10px] px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-700 text-gray-500">
              {c}
            </span>
          ))}
        </div>
      )}

      {/* Files */}
      {(detail.files_read?.length > 0 || detail.files_modified?.length > 0) && (
        <div className="mt-2 text-[11px] font-mono text-gray-400 space-y-0.5">
          {detail.files_read?.length > 0 && (
            <div>read: {detail.files_read.join(', ')}</div>
          )}
          {detail.files_modified?.length > 0 && (
            <div>modified: {detail.files_modified.join(', ')}</div>
          )}
        </div>
      )}

      <div className="mt-1 text-[11px] font-mono text-gray-400">
        #{detail.id} • {formatDate(detail.created_at)} {formatTime(detail.created_at)}
      </div>
    </div>
  )
}

export function TimelinePage({ project }: { project: string }) {
  const [items, setItems] = useState<TimelineItem[]>([])
  const [typeFilter, setTypeFilter] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [obsDetails, setObsDetails] = useState<Record<number, ObservationDetail>>({})
  const [loading, setLoading] = useState(false)
  const [showSummaries, setShowSummaries] = useState(true)
  const [showPrompts, setShowPrompts] = useState(true)

  useEffect(() => {
    if (!project) return
    setLoading(true)
    setExpanded(new Set())
    setObsDetails({})

    const obsPromise = listObservations(project, typeFilter || undefined, 100)
    const sumPromise = showSummaries && !typeFilter
      ? listSummaries(project, 50)
      : Promise.resolve({ summaries: null, total: 0 })
    const promptPromise = showPrompts && !typeFilter
      ? listPrompts(project, 50)
      : Promise.resolve({ prompts: null, total: 0 })

    Promise.all([obsPromise, sumPromise, promptPromise])
      .then(([obsRes, sumRes, promptRes]) => {
        const combined: TimelineItem[] = []
        for (const o of obsRes.results || []) combined.push({ kind: 'observation', data: o })
        for (const s of sumRes.summaries || []) combined.push({ kind: 'summary', data: s })
        for (const p of promptRes.prompts || []) combined.push({ kind: 'prompt', data: p })
        combined.sort((a, b) => new Date(b.data.created_at).getTime() - new Date(a.data.created_at).getTime())
        setItems(combined)
      })
      .finally(() => setLoading(false))
  }, [project, typeFilter, showSummaries, showPrompts])

  const toggle = (key: string, obsId?: number) => {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(key)) {
        next.delete(key)
      } else {
        next.add(key)
        // Fetch observation detail on expand
        if (obsId && !obsDetails[obsId]) {
          getObservation(obsId).then((d) => setObsDetails((prev) => ({ ...prev, [obsId]: d })))
        }
      }
      return next
    })
  }

  // Group by date
  const groups: { date: string; items: TimelineItem[] }[] = []
  for (const item of items) {
    const d = formatDate(item.data.created_at)
    const last = groups[groups.length - 1]
    if (last?.date === d) last.items.push(item)
    else groups.push({ date: d, items: [item] })
  }

  if (!project) {
    return <p className="text-gray-500">Select a project to view timeline.</p>
  }

  return (
    <div>
      <div className="flex gap-2 mb-4 flex-wrap items-center">
        {obsTypes.map((t) => (
          <button key={t} onClick={() => setTypeFilter(t)}
            className={`px-3 py-1 text-xs rounded-full border ${typeFilter === t ? 'bg-blue-500 text-white border-blue-500' : 'border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700'}`}>
            {t || 'All'}
          </button>
        ))}
        <span className="border-l border-gray-300 dark:border-gray-600 h-5 mx-1" />
        <label className="flex items-center gap-1.5 text-xs text-gray-500 cursor-pointer">
          <input type="checkbox" checked={showSummaries} onChange={(e) => setShowSummaries(e.target.checked)} className="rounded" />
          Summaries
        </label>
        <label className="flex items-center gap-1.5 text-xs text-gray-500 cursor-pointer">
          <input type="checkbox" checked={showPrompts} onChange={(e) => setShowPrompts(e.target.checked)} className="rounded" />
          Prompts
        </label>
      </div>

      {loading && <p className="text-gray-500">Loading...</p>}

      {groups.map((g) => (
        <div key={g.date} className="mb-6">
          <h3 className="text-sm font-semibold text-gray-500 dark:text-gray-400 mb-2 border-b border-gray-200 dark:border-gray-700 pb-1">
            {g.date}
          </h3>
          <div className="space-y-2">
            {g.items.map((item) => {
              if (item.kind === 'prompt') {
                const p = item.data
                const key = `prompt-${p.id}`
                const isExpanded = expanded.has(key)
                return (
                  <div key={key} onClick={() => toggle(key)}
                    className="bg-white dark:bg-gray-800 rounded-lg border border-cyan-200 dark:border-cyan-800 p-3 cursor-pointer hover:border-cyan-400 dark:hover:border-cyan-600 transition-colors">
                    <div className="flex items-center gap-2">
                      <span className="text-xs text-gray-400">{formatTime(p.created_at)}</span>
                      <span className={`text-xs px-2 py-0.5 rounded-full ${typeColors.prompt}`}>prompt #{p.prompt_number}</span>
                      {!isExpanded && <span className="text-sm text-gray-700 dark:text-gray-300 truncate">{truncatePrompt(p.prompt)}</span>}
                    </div>
                    {isExpanded && (
                      <pre className="mt-2 text-sm text-gray-600 dark:text-gray-400 pl-4 border-l-2 border-cyan-200 dark:border-cyan-700 whitespace-pre-wrap overflow-x-auto max-h-96 overflow-y-auto">
                        {p.prompt}
                      </pre>
                    )}
                  </div>
                )
              }

              if (item.kind === 'summary') {
                const s = item.data
                const key = `summary-${s.id}`
                const isExpanded = expanded.has(key)
                return (
                  <div key={key} onClick={() => toggle(key)}
                    className="bg-white dark:bg-gray-800 rounded-lg border-2 border-dashed border-amber-300 dark:border-amber-700 p-4 cursor-pointer hover:border-amber-400 dark:hover:border-amber-500 transition-colors">
                    <div className="flex items-center gap-2 mb-1">
                      <span className="text-xs text-gray-400">{formatTime(s.created_at)}</span>
                      <span className={`text-xs px-2 py-0.5 rounded-full ${typeColors.summary}`}>session summary</span>
                      <span className="text-xs text-gray-400">{s.project}</span>
                    </div>
                    <h4 className="font-semibold text-amber-800 dark:text-amber-300">{s.request || 'Session Summary'}</h4>
                    {isExpanded && (
                      <div className="mt-3 pl-4 border-l-2 border-amber-200 dark:border-amber-700 space-y-1">
                        <SummarySection label="Investigated" text={s.investigated} />
                        <SummarySection label="Learned" text={s.learned} />
                        <SummarySection label="Completed" text={s.completed} />
                        <SummarySection label="Next Steps" text={s.next_steps} />
                        {s.notes && <SummarySection label="Notes" text={s.notes} />}
                        <div className="mt-1 text-[11px] font-mono text-gray-400">
                          #{s.id} • {formatDate(s.created_at)} {formatTime(s.created_at)}
                        </div>
                      </div>
                    )}
                  </div>
                )
              }

              const r = item.data
              const key = `obs-${r.id}`
              const detail = obsDetails[r.id]
              return (
                <div key={key} onClick={() => toggle(key, r.id)}
                  className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-3 cursor-pointer hover:border-blue-300 dark:hover:border-blue-600 transition-colors">
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-gray-400">{formatTime(r.created_at)}</span>
                    <span className={`text-xs px-2 py-0.5 rounded-full ${typeColors[r.type] || 'bg-gray-100 text-gray-600'}`}>{r.type}</span>
                    <span className="font-medium text-sm">{r.title}</span>
                  </div>
                  {expanded.has(key) && detail && <ObsDetail detail={detail} />}
                  {expanded.has(key) && !detail && (
                    <p className="mt-2 text-sm text-gray-400 pl-4">Loading...</p>
                  )}
                </div>
              )
            })}
          </div>
        </div>
      ))}

      {!loading && items.length === 0 && (
        <p className="text-gray-500">No data found.</p>
      )}
    </div>
  )
}
