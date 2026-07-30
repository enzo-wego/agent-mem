import { useState } from 'react'
import {
  search,
  graphResolve,
  parseSlackLink,
  type SearchResult,
  type ResolveArtifact,
} from '../api'

export function SearchPage({ project }: { project: string }) {
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<SearchResult[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [searched, setSearched] = useState(false)
  // Slack-thread-link mode: the graph-memory linked to a pasted Slack thread.
  const [thread, setThread] = useState<ResolveArtifact | null>(null)
  const [linked, setLinked] = useState<ResolveArtifact[]>([])
  const [threadErr, setThreadErr] = useState('')

  const doSearch = async () => {
    if (!query.trim()) return
    setLoading(true)
    setSearched(true)
    // Reset both modes.
    setThread(null)
    setLinked([])
    setThreadErr('')
    setResults([])

    const slack = parseSlackLink(query.trim())
    try {
      if (slack) {
        // Graph-memory view: resolve the thread node + its linked neighbors.
        const res = await graphResolve([slack.nodeId], undefined, 2)
        const arts = res.artifacts || []
        const root = arts.find((a) => a.hop === 0) ?? null
        const rest = arts.filter((a) => a !== root).sort((a, b) => (b.score ?? 0) - (a.score ?? 0))
        setThread(root)
        setLinked(rest)
        if (!root && rest.length === 0) {
          setThreadErr(`Thread not found in the graph (${slack.nodeId}). It may not be ingested yet, or the link points to a reply rather than the thread root.`)
        }
        setTotal(arts.length)
        return
      }
      const res = await search(query, project || undefined)
      setResults(res.results || [])
      setTotal(res.total)
    } finally {
      setLoading(false)
    }
  }

  const isSlack = !!parseSlackLink(query.trim())

  return (
    <div>
      <div className="flex gap-2 mb-2">
        <input
          type="text"
          placeholder="Search observations and summaries — or paste a Slack thread link…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && doSearch()}
          className="flex-1 px-4 py-2 border border-gray-300 dark:border-gray-600 rounded-lg bg-white dark:bg-gray-800 focus:outline-none focus:ring-2 focus:ring-blue-500"
        />
        <button
          onClick={doSearch}
          disabled={loading}
          className="px-6 py-2 bg-blue-500 text-white rounded-lg hover:bg-blue-600 disabled:opacity-50"
        >
          {loading ? '...' : 'Search'}
        </button>
      </div>
      {isSlack && (
        <p className="text-xs text-blue-500 mb-4">
          Slack thread link detected — showing graph memory linked to this thread.
        </p>
      )}

      {searched && !isSlack && (
        <p className="text-sm text-gray-500 mb-4 mt-4">{total} results</p>
      )}

      {/* ── Slack thread mode ── */}
      {threadErr && (
        <div className="bg-yellow-50 dark:bg-yellow-900/20 border border-yellow-200 dark:border-yellow-800 rounded-lg p-4 text-sm text-yellow-800 dark:text-yellow-300">
          {threadErr}
        </div>
      )}
      {thread && (
        <div className="mb-4 bg-white dark:bg-gray-800 rounded-lg border border-blue-300 dark:border-blue-700 p-4">
          <div className="flex items-center gap-2 mb-1">
            <span className="text-xs px-2 py-0.5 rounded-full bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300">
              thread
            </span>
            {thread.author && <span className="text-xs text-gray-400">{thread.author}</span>}
          </div>
          <h4 className="font-medium text-sm">{thread.title || thread.node_id}</h4>
          {thread.url && (
            <a href={thread.url} target="_blank" rel="noreferrer" className="text-xs text-blue-500 hover:underline">
              open in Slack ↗
            </a>
          )}
        </div>
      )}
      {(thread || linked.length > 0) && (
        <p className="text-sm text-gray-500 mb-2">
          {linked.length} linked {linked.length === 1 ? 'item' : 'items'} in graph memory
        </p>
      )}
      <div className="space-y-3">
        {linked.map((a) => (
          <div key={a.node_id} className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
            <div className="flex items-center gap-2 mb-1">
              <span className="text-xs px-2 py-0.5 rounded-full bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400">
                {a.type}
              </span>
              <span className="text-xs text-gray-400">hop {a.hop}</span>
              {a.score !== undefined && (
                <span className="text-xs text-gray-400">score: {a.score.toFixed(2)}</span>
              )}
              {a.author && <span className="text-xs text-gray-400">{a.author}</span>}
            </div>
            <h4 className="font-medium text-sm">{a.title || a.node_id}</h4>
            {a.url && (
              <a href={a.url} target="_blank" rel="noreferrer" className="text-xs text-blue-500 hover:underline">
                {a.url}
              </a>
            )}
          </div>
        ))}
      </div>

      {/* ── Keyword search mode ── */}
      <div className="space-y-3">
        {results.map((r) => (
          <div key={`${r.type}-${r.id}`} className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
            <div className="flex items-center gap-2 mb-1">
              <span className="text-xs px-2 py-0.5 rounded-full bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400">
                {r.type}
              </span>
              <span className="text-xs text-gray-400">
                {new Date(r.created_at).toLocaleDateString()}
              </span>
              {r.combined_score !== undefined && r.combined_score > 0 && (
                <span className="text-xs text-gray-400">
                  score: {r.combined_score.toFixed(2)}
                </span>
              )}
            </div>
            <h4 className="font-medium text-sm">{r.title}</h4>
            {r.subtitle && (
              <p className="text-sm text-gray-500 mt-0.5">{r.subtitle}</p>
            )}
            {r.narrative && (
              <p className="text-sm text-gray-600 dark:text-gray-400 mt-2">
                {r.narrative}
              </p>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}
