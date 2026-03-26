import { useState } from 'react'
import { search, type SearchResult } from '../api'

export function SearchPage({ project }: { project: string }) {
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<SearchResult[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [searched, setSearched] = useState(false)

  const doSearch = async () => {
    if (!query.trim()) return
    setLoading(true)
    setSearched(true)
    try {
      const res = await search(query, project || undefined)
      setResults(res.results || [])
      setTotal(res.total)
    } finally {
      setLoading(false)
    }
  }

  return (
    <div>
      <div className="flex gap-2 mb-6">
        <input
          type="text"
          placeholder="Search observations and summaries..."
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

      {searched && (
        <p className="text-sm text-gray-500 mb-4">{total} results</p>
      )}

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
