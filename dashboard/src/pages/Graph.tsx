import { useEffect, useRef, useState, type ReactNode } from 'react'
import ForceGraph2D from 'react-force-graph-2d'
import { graphSearch, graphResolve, graphNode, graphNeighbors, graphSlackUsers, type GraphNode, type ResolveArtifact } from '../api'

// Slack user-id → display name, loaded once from /api/graph/slack-users.
let slackUserMap: Record<string, string> = {}

// ── Slack-markup helpers ──────────────────────────────────────────────────────
// Slack stores mentions/links as <@U…>, <#C…|name>, <url|label>. Render them as
// links / clean text instead of raw angle-bracket markup.
const SLACK_MARKUP = /<@(U[A-Z0-9]+)>|<#C[A-Z0-9]+\|([^>]+)>|<(https?:\/\/[^>|]+)(?:\|([^>]+))?>/g

function renderSlackText(text: string): ReactNode[] {
  const out: ReactNode[] = []
  let last = 0, key = 0
  let m: RegExpExecArray | null
  SLACK_MARKUP.lastIndex = 0
  while ((m = SLACK_MARKUP.exec(text)) !== null) {
    if (m.index > last) out.push(text.slice(last, m.index))
    const link = 'text-blue-500 hover:underline'
    if (m[1]) {
      const name = slackUserMap[m[1]] || m[1]
      out.push(<a key={key++} href={`https://wego.slack.com/team/${m[1]}`} target="_blank" rel="noopener noreferrer" className={link}>@{name}</a>)
    } else if (m[2]) {
      out.push('#' + m[2])
    } else if (m[3]) {
      out.push(<a key={key++} href={m[3]} target="_blank" rel="noopener noreferrer" className={link}>{m[4] || m[3]}</a>)
    }
    last = SLACK_MARKUP.lastIndex
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

// Plain-text cleanup of the same markup, for compact labels.
function cleanSlack(s: string): string {
  return s
    .replace(/<@(U[A-Z0-9]+)>/g, (_, uid) => '@' + (slackUserMap[uid] || uid))
    .replace(/<#C[A-Z0-9]+\|([^>]+)>/g, '#$1')
    .replace(/<(https?:\/\/[^>|]+)(?:\|([^>]+))?>/g, (_, u, l) => l || u)
}

// Short, human-readable label for a graph node: prefer title, else type + key.
function shortLabel(title: string | undefined, id: string, type: string): string {
  const t = cleanSlack((title || '').trim())
  if (t) return t.length > 38 ? t.slice(0, 38) + '…' : t
  const seg = id.split(':').pop() || id
  return `${type} ${seg.slice(0, 12)}`
}

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
              {renderSlackText(detail.body.slice(0, 1000))}{detail.body.length > 1000 ? '…' : ''}
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

function SearchResultCard({ node, onVisualize }: { node: GraphNode; onVisualize?: (id: string) => void }) {
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
      <h4 className="font-medium text-sm mb-1">{cleanSlack(node.title || node.id)}</h4>
      {node.body && (
        <p className="text-xs text-gray-500 dark:text-gray-400 line-clamp-2">
          {renderSlackText(node.body.slice(0, 200))}{node.body.length > 200 ? '…' : ''}
        </p>
      )}
      <div className="mt-2 flex gap-3">
        <button
          onClick={() => setExpanded(!expanded)}
          className="text-xs text-blue-500 hover:underline"
        >
          {expanded ? 'Hide detail' : 'View node'}
        </button>
        {onVisualize && (
          <button
            onClick={() => onVisualize(node.id)}
            className="text-xs text-blue-500 hover:underline"
          >
            Visualize
          </button>
        )}
      </div>
      {expanded && <NodeDetailPanel nodeId={node.id} onClose={() => setExpanded(false)} />}
    </div>
  )
}

function SearchTab({ onVisualize }: { onVisualize?: (id: string) => void }) {
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
          <SearchResultCard key={node.id} node={node} onVisualize={onVisualize} />
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
                  <p className="text-sm font-medium">{cleanSlack(a.title || a.node_id)}</p>
                  {a.body && (
                    <p className="text-xs text-gray-500 mt-1 line-clamp-2">
                      {renderSlackText(a.body.slice(0, 200))}{a.body.length > 200 ? '…' : ''}
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

// ── Graph visualization tab (Obsidian-style local graph) ─────────────────────

type VizNode = { id: string; label: string; type: string }
type VizLink = { source: string; target: string; kind: string }

const TYPE_COLORS: Record<string, string> = {
  slack: '#a855f7', slack_thread: '#a855f7', slack_file: '#eab308',
  jira: '#3b82f6', gh_pr: '#9ca3af', cf: '#06b6d4', cf_page: '#06b6d4',
  pagerduty: '#22c55e', datadog: '#f97316', sentry: '#ef4444',
  gws_doc: '#14b8a6', wegohub: '#84cc16', claude_artifact: '#f59e0b',
}
const colorFor = (t: string) => TYPE_COLORS[t] ?? '#6b7280'

function GraphVizTab({ initialSeed }: { initialSeed?: string }) {
  const [seed, setSeed] = useState(initialSeed ?? '')
  const [data, setData] = useState<{ nodes: VizNode[]; links: VizLink[] }>({ nodes: [], links: [] })
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [seedTitle, setSeedTitle] = useState('')
  const expanded = useRef<Set<string>>(new Set())
  const wrapRef = useRef<HTMLDivElement>(null)
  const [width, setWidth] = useState(800)

  useEffect(() => {
    const measure = () => { if (wrapRef.current) setWidth(wrapRef.current.clientWidth) }
    measure()
    window.addEventListener('resize', measure)
    return () => window.removeEventListener('resize', measure)
  }, [])

  const expand = async (id: string, seeding = false, title = '') => {
    if (expanded.current.has(id)) return
    setLoading(true); setError('')
    try {
      const nbrs = await graphNeighbors(id, 1)
      expanded.current.add(id)
      setData((prev) => {
        const nodes = [...prev.nodes]
        const links = [...prev.links]
        const have = new Set(nodes.map((n) => n.id))
        const seedType = id.split(':')[0]
        if (seeding && !have.has(id)) { nodes.push({ id, label: shortLabel(title, id, seedType), type: seedType }); have.add(id) }
        const linkSet = new Set(links.map((l) => `${l.source}->${l.target}`))
        for (const nb of nbrs) {
          const nid = nb.node.node_id
          if (!have.has(nid)) { nodes.push({ id: nid, label: shortLabel(nb.node.title, nid, nb.node.type), type: nb.node.type }); have.add(nid) }
          const key = `${id}->${nid}`
          if (!linkSet.has(key)) { links.push({ source: id, target: nid, kind: nb.edge.kind }); linkSet.add(key) }
        }
        return { nodes, links }
      })
    } catch (e) { setError(e instanceof Error ? e.message : 'Failed to load neighbors') }
    finally { setLoading(false) }
  }

  const start = async (id: string) => {
    if (!id.trim()) return
    const sid = id.trim()
    expanded.current = new Set()
    setData({ nodes: [], links: [] })
    setSeedTitle('')
    let title = ''
    try { const d = await graphNode(undefined, sid); title = d.title || '' } catch { /* ignore */ }
    setSeedTitle(shortLabel(title, sid, sid.split(':')[0]))
    await expand(sid, true, title)
  }

  useEffect(() => { if (initialSeed) { setSeed(initialSeed); start(initialSeed) } }, [initialSeed]) // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="space-y-3">
      <div className="flex gap-2">
        <input
          type="text"
          placeholder="Seed node id (e.g. jira:PAY-2190 or slack:C05RNSE8TBR:1779…)"
          value={seed}
          onChange={(e) => setSeed(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && start(seed)}
          className="flex-1 px-4 py-2 border border-gray-300 dark:border-gray-600 rounded-lg bg-white dark:bg-gray-800 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500"
        />
        <button onClick={() => start(seed)} disabled={loading} className="px-6 py-2 bg-blue-500 text-white rounded-lg hover:bg-blue-600 disabled:opacity-50 text-sm">
          {loading ? '...' : 'Load'}
        </button>
      </div>
      {error && <p className="text-red-500 text-sm">{error}</p>}
      {seedTitle && <p className="text-sm font-medium text-gray-700 dark:text-gray-300">Seed: {seedTitle}</p>}
      <p className="text-xs text-gray-500">
        {data.nodes.length} nodes · {data.links.length} edges — click a node to expand its neighbors, drag to rearrange, scroll to zoom.
      </p>
      <div ref={wrapRef} className="border border-gray-200 dark:border-gray-700 rounded-lg overflow-hidden bg-gray-50 dark:bg-gray-900" style={{ height: 560 }}>
        {data.nodes.length > 0 && (
          <ForceGraph2D
            graphData={data}
            width={width}
            height={560}
            nodeId="id"
            nodeLabel={(n: VizNode) => `${n.type}: ${n.label}`}
            nodeColor={(n: VizNode) => colorFor(n.type)}
            nodeRelSize={5}
            linkColor={() => 'rgba(148,163,184,0.4)'}
            linkDirectionalArrowLength={3}
            linkDirectionalArrowRelPos={1}
            onNodeClick={(n: VizNode) => expand(n.id)}
            nodeCanvasObjectMode={() => 'after'}
            nodeCanvasObject={(n: VizNode & { x?: number; y?: number }, ctx, scale) => {
              if (scale < 1.5) return
              const label = n.label.length > 28 ? n.label.slice(0, 28) + '…' : n.label
              ctx.font = `${10 / scale}px sans-serif`
              ctx.fillStyle = 'rgba(120,130,145,0.9)'
              ctx.textAlign = 'center'
              ctx.fillText(label, n.x ?? 0, (n.y ?? 0) + 8)
            }}
          />
        )}
      </div>
    </div>
  )
}

// ── Main page ────────────────────────────────────────────────────────────────

type GraphTab = 'search' | 'resolve' | 'graph'

export function GraphPage() {
  const [tab, setTab] = useState<GraphTab>('search')
  const [vizSeed, setVizSeed] = useState<string | undefined>(undefined)
  const [, bumpUsers] = useState(0)

  useEffect(() => {
    graphSlackUsers().then((m) => { slackUserMap = m; bumpUsers((n) => n + 1) }).catch(() => {})
  }, [])

  const visualize = (id: string) => { setVizSeed(id); setTab('graph') }

  return (
    <div className="space-y-4">
      <div className="flex gap-0 border-b border-gray-200 dark:border-gray-700">
        {(['search', 'resolve', 'graph'] as GraphTab[]).map((t) => (
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

      {tab === 'search' && <SearchTab onVisualize={visualize} />}
      {tab === 'resolve' && <ResolveTab />}
      {tab === 'graph' && <GraphVizTab initialSeed={vizSeed} />}
    </div>
  )
}
