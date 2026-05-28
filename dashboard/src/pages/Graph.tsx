import { useEffect, useState } from 'react'
import { graphSearch, graphResolve, graphNode, type GraphNode, type ResolveArtifact } from '../api'

const NODE_TYPES = [
  'slack_thread',
  'jira',
  'gh_pr',
  'cf_page',
  'pagerduty',
  'datadog',
  'sentry',
  'slack_file',
]

function TypeBadge({ type }: { type: string }) {
  const colors: Record<string, string> = {
    slack_thread: 'bg-purple-100 text-purple-800 dark:bg-purple-900/30 dark:text-purple-300',
    jira: 'bg-blue-100 text-blue-800 dark:bg-blue-900/30 dark:text-blue-300',
    gh_pr: 'bg-gray-100 text-gray-800 dark:bg-gray-700 dark:text-gray-300',
    cf_page: 'bg-cyan-100 text-cyan-800 dark:bg-cyan-900/30 dark:text-cyan-300',
    pagerduty: 'bg-green-100 text-green-800 dark:bg-green-900/30 dark:text-green-300',
    datadog: 'bg-orange-100 text-orange-800 dark:bg-orange-900/30 dark:text-orange-300',
    sentry: 'bg-red-100 text-red-800 dark:bg-red-900/30 dark:text-red-300',
    slack_file: 'bg-yellow-100 text-yellow-800 dark:bg-yellow-900/30 dark:text-yellow-300',
  }
  return (
    <span className={`text-xs px-2 py-0.5 rounded-full font-medium ${colors[type] ?? 'bg-gray-100 text-gray-700 dark:bg-gray-700 dark:text-gray-300'}`}>
      {type}
    </span>
  )
}

// ── Search tab ──────────────────────────────────────────────────────────────

function NodeDetailPanel({ nodeId, onClose }: { nodeId: string; onClose: () => void }) {
  const [detail, setDetail] = useState<{ title?: string; url?: string; body?: string; metadata?: Record<string, unknown> } | null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  useEffect(() => {
    graphNode(undefined, nodeId)
      .then(setDetail)
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : 'Failed'))
      .finally(() => setLoading(false))
  }, [nodeId])

  return (
    <div className="mt-3 border-t border-gray-200 dark:border-gray-700 pt-3">
      <div className="flex justify-between items-start mb-2">
        <span className="text-xs font-medium text-gray-500">Node detail</span>
        <button onClick={onClose} className="text-xs text-gray-400 hover:text-gray-600">close</button>
      </div>
      {loading && <p className="text-xs text-gray-400">Loading...</p>}
      {err && <p className="text-xs text-red-500">{err}</p>}
      {detail && (
        <div className="space-y-2 text-xs">
          {detail.url && (
            <p className="text-gray-500">
              <a href={detail.url} target="_blank" rel="noopener noreferrer" className="text-blue-500 hover:underline break-all">
                {detail.url}
              </a>
            </p>
          )}
          {detail.body && (
            <pre className="whitespace-pre-wrap break-all text-gray-700 dark:text-gray-300 bg-gray-50 dark:bg-gray-900 rounded p-2 max-h-40 overflow-auto">
              {detail.body.slice(0, 1000)}{detail.body.length > 1000 ? '…' : ''}
            </pre>
          )}
          {detail.metadata && (
            <pre className="whitespace-pre-wrap break-all text-gray-500 text-xs">
              {JSON.stringify(detail.metadata, null, 2).slice(0, 500)}
            </pre>
          )}
        </div>
      )}
    </div>
  )
}

function SearchResultCard({ node }: { node: GraphNode }) {
  const [expanded, setExpanded] = useState(false)

  return (
    <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
      <div className="flex items-center gap-2 mb-2 flex-wrap">
        <TypeBadge type={node.type} />
        {node.author && (
          <span className="text-xs bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400 px-2 py-0.5 rounded-full">
            {node.author}
          </span>
        )}
        {node.score !== undefined && (
          <span className="text-xs text-gray-400 ml-auto">
            score {node.score.toFixed(3)}
            {node.score_breakdown && (
              <span className="ml-1 text-gray-300 dark:text-gray-600" title={JSON.stringify(node.score_breakdown, null, 2)}>
                (?)
              </span>
            )}
          </span>
        )}
      </div>
      <h4 className="font-medium text-sm mb-1">{node.title || node.id}</h4>
      {node.body && (
        <p className="text-xs text-gray-500 dark:text-gray-400 line-clamp-2">
          {node.body.slice(0, 200)}{node.body.length > 200 ? '…' : ''}
        </p>
      )}
      <div className="mt-2">
        <button
          onClick={() => setExpanded(!expanded)}
          className="text-xs text-blue-500 hover:underline"
        >
          {expanded ? 'Hide detail' : 'View node'}
        </button>
      </div>
      {expanded && <NodeDetailPanel nodeId={node.id} onClose={() => setExpanded(false)} />}
    </div>
  )
}

function SearchTab() {
  const [query, setQuery] = useState('')
  const [selectedTypes, setSelectedTypes] = useState<string[]>([])
  const [results, setResults] = useState<GraphNode[]>([])
  const [loading, setLoading] = useState(false)
  const [searched, setSearched] = useState(false)
  const [error, setError] = useState('')

  const toggleType = (t: string) => {
    setSelectedTypes((prev) =>
      prev.includes(t) ? prev.filter((x) => x !== t) : [...prev, t],
    )
  }

  const doSearch = async () => {
    if (!query.trim()) return
    setLoading(true)
    setError('')
    setSearched(true)
    try {
      const res = await graphSearch(query.trim(), selectedTypes.length > 0 ? selectedTypes : undefined)
      setResults(res.results ?? [])
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Search failed')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex gap-2">
        <input
          type="text"
          placeholder="Search the graph..."
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

      {/* Type filter */}
      <div className="flex flex-wrap gap-2">
        {NODE_TYPES.map((t) => (
          <button
            key={t}
            onClick={() => toggleType(t)}
            className={`text-xs px-2 py-0.5 rounded-full border transition-colors ${
              selectedTypes.includes(t)
                ? 'bg-blue-500 text-white border-blue-500'
                : 'border-gray-300 dark:border-gray-600 text-gray-600 dark:text-gray-400 hover:border-blue-400'
            }`}
          >
            {t}
          </button>
        ))}
        {selectedTypes.length > 0 && (
          <button
            onClick={() => setSelectedTypes([])}
            className="text-xs px-2 py-0.5 rounded-full border border-gray-300 text-gray-400 hover:border-red-400"
          >
            clear filters
          </button>
        )}
      </div>

      {error && <p className="text-red-500 text-sm">{error}</p>}
      {searched && !loading && (
        <p className="text-sm text-gray-500">{results.length} result{results.length !== 1 ? 's' : ''}</p>
      )}

      <div className="space-y-3">
        {results.map((node) => (
          <SearchResultCard key={node.id} node={node} />
        ))}
      </div>
    </div>
  )
}

// ── Resolve tab ─────────────────────────────────────────────────────────────

function ResolveTab() {
  const [seedUrl, setSeedUrl] = useState('')
  const [queryText, setQueryText] = useState('')
  const [depth, setDepth] = useState(2)
  const [budget, setBudget] = useState(4000)
  const [artifacts, setArtifacts] = useState<ResolveArtifact[]>([])
  const [trace, setTrace] = useState<{ expanded_nodes: number; after_acl: number; took_ms: number; cache_misses?: string[] } | null>(null)
  const [loading, setLoading] = useState(false)
  const [resolved, setResolved] = useState(false)
  const [error, setError] = useState('')

  const doResolve = async () => {
    if (!seedUrl.trim()) return
    setLoading(true)
    setError('')
    setResolved(true)
    try {
      const res = await graphResolve([seedUrl.trim()], queryText.trim() || undefined, depth, budget)
      setArtifacts(res.artifacts ?? [])
      setTrace(res.trace ?? null)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Resolve failed')
    } finally {
      setLoading(false)
    }
  }

  // Group by hop.
  const byHop: Record<number, ResolveArtifact[]> = {}
  for (const a of artifacts) {
    if (!byHop[a.hop]) byHop[a.hop] = []
    byHop[a.hop].push(a)
  }
  const hopLabels: Record<number, string> = { 0: 'Seed', 1: 'Direct neighbours', 2: 'Second-hop' }

  return (
    <div className="space-y-4">
      <div className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-4 space-y-3">
        <div>
          <label className="block text-xs font-medium text-gray-500 mb-1">URL or graph ID (seed)</label>
          <input
            type="text"
            placeholder="https://wego.slack.com/archives/C08S954G2LX/p... or slack:C08S:1234.0"
            value={seedUrl}
            onChange={(e) => setSeedUrl(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && doResolve()}
            className="w-full px-3 py-2 border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500"
          />
        </div>
        <div>
          <label className="block text-xs font-medium text-gray-500 mb-1">Context query (optional)</label>
          <input
            type="text"
            placeholder="e.g. payment failures deployment"
            value={queryText}
            onChange={(e) => setQueryText(e.target.value)}
            className="w-full px-3 py-2 border border-gray-300 dark:border-gray-600 rounded-md bg-white dark:bg-gray-700 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500"
          />
        </div>
        <div className="flex gap-6 items-center">
          <div>
            <label className="text-xs text-gray-500 mr-2">Depth</label>
            <input
              type="range"
              min={1}
              max={3}
              value={depth}
              onChange={(e) => setDepth(Number(e.target.value))}
              className="align-middle"
            />
            <span className="text-xs text-gray-600 dark:text-gray-400 ml-1">{depth}</span>
          </div>
          <div>
            <label className="text-xs text-gray-500 mr-2">Token budget</label>
            <input
              type="number"
              min={500}
              max={16000}
              step={500}
              value={budget}
              onChange={(e) => setBudget(Number(e.target.value))}
              className="w-20 px-2 py-1 text-xs border border-gray-300 dark:border-gray-600 rounded bg-white dark:bg-gray-700 focus:outline-none"
            />
          </div>
          <button
            onClick={doResolve}
            disabled={loading}
            className="ml-auto px-4 py-2 bg-blue-500 text-white rounded-lg hover:bg-blue-600 disabled:opacity-50 text-sm"
          >
            {loading ? 'Resolving...' : 'Resolve'}
          </button>
        </div>
      </div>

      {error && <p className="text-red-500 text-sm">{error}</p>}

      {resolved && !loading && artifacts.length === 0 && !error && (
        <p className="text-gray-500 text-sm">No artifacts returned.</p>
      )}

      {/* Artifacts grouped by hop */}
      {Object.keys(byHop)
        .map(Number)
        .sort()
        .map((hop) => (
          <div key={hop}>
            <h3 className="text-xs font-semibold text-gray-500 uppercase tracking-wide mb-2">
              {hopLabels[hop] ?? `Hop ${hop}`}
            </h3>
            <div className="space-y-2">
              {byHop[hop].map((a) => (
                <div
                  key={a.node_id}
                  className="bg-white dark:bg-gray-800 rounded-lg border border-gray-200 dark:border-gray-700 p-3"
                >
                  <div className="flex items-center gap-2 mb-1 flex-wrap">
                    <TypeBadge type={a.type} />
                    {a.author && (
                      <span className="text-xs bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-400 px-2 py-0.5 rounded-full">
                        {a.author}
                      </span>
                    )}
                    {a.score !== undefined && (
                      <span className="text-xs text-gray-400 ml-auto">score {a.score.toFixed(3)}</span>
                    )}
                  </div>
                  <p className="text-sm font-medium">{a.title || a.node_id}</p>
                  {a.body && (
                    <p className="text-xs text-gray-500 mt-1 line-clamp-2">
                      {a.body.slice(0, 200)}{a.body.length > 200 ? '…' : ''}
                    </p>
                  )}
                  {a.url && (
                    <a
                      href={a.url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-xs text-blue-500 hover:underline mt-1 inline-block"
                    >
                      {a.url}
                    </a>
                  )}
                </div>
              ))}
            </div>
          </div>
        ))}

      {/* Graph trace */}
      {trace && (
        <div className="bg-gray-50 dark:bg-gray-800/50 rounded-lg border border-gray-200 dark:border-gray-700 p-4">
          <h3 className="text-xs font-semibold text-gray-500 uppercase tracking-wide mb-3">Graph trace</h3>
          <dl className="grid grid-cols-3 gap-4 text-sm mb-3">
            <div>
              <dt className="text-gray-400 text-xs">Expanded nodes</dt>
              <dd className="font-medium">{trace.expanded_nodes}</dd>
            </div>
            <div>
              <dt className="text-gray-400 text-xs">After ACL</dt>
              <dd className="font-medium">{trace.after_acl}</dd>
            </div>
            <div>
              <dt className="text-gray-400 text-xs">Took</dt>
              <dd className="font-medium">{trace.took_ms}ms</dd>
            </div>
          </dl>
          {trace.cache_misses && trace.cache_misses.length > 0 && (
            <div>
              <p className="text-xs text-gray-500 mb-1">Cache misses ({trace.cache_misses.length})</p>
              <div className="flex flex-wrap gap-1">
                {trace.cache_misses.map((id) => (
                  <span key={id} className="text-xs font-mono bg-gray-100 dark:bg-gray-700 px-1.5 py-0.5 rounded">
                    {id}
                  </span>
                ))}
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

// ── Main page ────────────────────────────────────────────────────────────────

type GraphTab = 'search' | 'resolve'

export function GraphPage() {
  const [tab, setTab] = useState<GraphTab>('search')

  return (
    <div className="space-y-4">
      <div className="flex gap-0 border-b border-gray-200 dark:border-gray-700">
        {(['search', 'resolve'] as GraphTab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`px-4 py-2 text-sm font-medium border-b-2 -mb-px transition-colors ${
              tab === t
                ? 'border-blue-500 text-blue-600 dark:text-blue-400'
                : 'border-transparent text-gray-500 hover:text-gray-700 dark:hover:text-gray-300'
            }`}
          >
            {t.charAt(0).toUpperCase() + t.slice(1)}
          </button>
        ))}
      </div>

      {tab === 'search' && <SearchTab />}
      {tab === 'resolve' && <ResolveTab />}
    </div>
  )
}
