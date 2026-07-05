import { useEffect, useMemo, useRef, useState } from 'react'
import {
  fetchChannels,
  fetchRecentActivity,
  fetchContinents,
  fetchChannelMessages,
  fetchChannelTopics,
  fetchNeighbors,
  fetchClusterSummary,
  graphSearch,
  listSubscriptions,
  createSubscription,
  deleteSubscription,
  refreshSubscription,
  updateSubscription,
  type ChannelCount,
  type ContinentCfg,
  type ChannelMessage,
  type ChannelTopic,
  type GraphNeighbor,
  type ClusterSummary,
  type GraphNode,
  type TopicSubscription,
  type TopicSource,
} from '../api'
import { applyGroupNames, assignCountries, continentOf, nameOf } from '../continents'
import ClusterGraph from './ClusterGraph'

// ── worldmonitor palette ──────────────────────────────────────────────────────
const C = {
  bg: '#0a0a0a',
  panel: '#141414',
  border: '#2a2a2a',
  text: '#e8e8e8',
  dim: '#888888',
  green: '#44ff88',
  amber: '#ffaa00',
  red: '#ff4444',
  land: '#141a17',
  landStroke: '#2a2f36',
} as const

const MONO = 'ui-monospace, "SF Mono", Menlo, monospace'

// Knowledge-source types a topic can be defined from (matches the graph fetchers).
const SOURCE_TYPES: { value: string; label: string }[] = [
  { value: 'confluence', label: 'Confluence' },
  { value: 'github', label: 'GitHub repo' },
  { value: 'slack', label: 'Slack' },
  { value: 'gws', label: 'Google Docs' },
  { value: 'wegohub', label: 'Wego Hub' },
  { value: 'claude_artifact', label: 'Claude Artifact' },
  { value: 'jira', label: 'Jira' },
]

// SourceRows renders the type-dropdown + URL editor rows plus a "+ add source"
// button. Shared by the create form and the per-subscription edit editor.
function SourceRows({
  sources,
  onUpdate,
  onRemove,
  onAdd,
}: {
  sources: TopicSource[]
  onUpdate: (i: number, patch: Partial<TopicSource>) => void
  onRemove: (i: number) => void
  onAdd: () => void
}) {
  return (
    <>
      {sources.map((src, i) => (
        <div key={i} style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
          <select
            value={src.type}
            onChange={(e) => onUpdate(i, { type: e.target.value })}
            style={{
              background: C.panel,
              border: `1px solid ${C.border}`,
              color: C.text,
              fontFamily: MONO,
              fontSize: 11,
              padding: '6px 6px',
              borderRadius: 2,
              outline: 'none',
            }}
          >
            {SOURCE_TYPES.map((o) => (
              <option key={o.value} value={o.value} style={{ background: C.panel }}>
                {o.label}
              </option>
            ))}
          </select>
          <input
            value={src.url}
            onChange={(e) => onUpdate(i, { url: e.target.value })}
            placeholder="URL (page / repo / message / doc …)"
            style={{
              flex: 1,
              minWidth: 0,
              background: C.panel,
              border: `1px solid ${C.border}`,
              color: C.text,
              fontFamily: MONO,
              fontSize: 11,
              padding: '6px 8px',
              borderRadius: 2,
              outline: 'none',
            }}
          />
          <button
            type="button"
            onClick={() => onRemove(i)}
            title="Remove source"
            style={{
              background: 'transparent',
              border: `1px solid ${C.border}`,
              color: C.dim,
              cursor: 'pointer',
              fontFamily: MONO,
              fontSize: 12,
              lineHeight: '12px',
              borderRadius: 2,
              padding: '5px 8px',
            }}
          >
            ✕
          </button>
        </div>
      ))}
      <button
        type="button"
        onClick={onAdd}
        style={{
          alignSelf: 'flex-start',
          background: 'transparent',
          border: `1px dashed ${C.border}`,
          color: C.dim,
          cursor: 'pointer',
          fontFamily: MONO,
          fontSize: 10,
          letterSpacing: '0.06em',
          borderRadius: 2,
          padding: '4px 10px',
        }}
      >
        + add source
      </button>
    </>
  )
}

// Friendly group heading + sort order for the "open in Graph" overlay. Lower
// `order` sorts first; unknown types fall back to the raw type and sort last.
const GRAPH_TYPE_GROUPS: Record<string, { label: string; order: number }> = {
  jira: { label: 'Jira', order: 0 },
  gh_pr: { label: 'Pull Requests', order: 1 },
  cf: { label: 'Confluence', order: 2 },
  cf_page: { label: 'Confluence', order: 2 },
  slack_thread: { label: 'Slack threads', order: 3 },
  slack: { label: 'Slack threads', order: 3 },
  slack_file: { label: 'Files', order: 4 },
  jira_attachment: { label: 'Attachments', order: 4 },
  person: { label: 'People', order: 5 },
  gws_doc: { label: 'Google Docs', order: 2 },
  gws: { label: 'Google Docs', order: 2 },
  wegohub: { label: 'Wego Hub', order: 2 },
  claude_artifact: { label: 'Claude Artifacts', order: 2 },
  feature: { label: 'Features', order: 6 },
}

// Short per-row identifier so same-titled rows stay distinguishable: the Jira
// key, the Confluence space, or the GitHub repo#num. '' when there's nothing
// more useful than the title itself.
function neighborHint(n: GraphNeighbor): string {
  const { type, node_id, url } = n.node
  if (type === 'jira') return node_id.slice('jira:'.length)
  if (type === 'gh_pr') return node_id.slice('gh_pr:'.length)
  if (type === 'cf' || type === 'cf_page') {
    const m = /\/spaces\/([^/]+)\//.exec(url || '')
    return m ? m[1] : ''
  }
  return ''
}

// edgeKindTooltip explains why a row is linked to the opened topic. SIMILAR rows
// have no explicit edge — they matched by embedding cosine (threshold 0.45 in
// bfs.SimilarThreads) — so show the match strength; other kinds are real edges.
function edgeKindTooltip(edge: { kind: string; score?: number }): string {
  switch (edge.kind.toUpperCase()) {
    case 'SIMILAR':
      return (
        `No direct link — matched by meaning: this thread's summary is ` +
        `${edge.score ? Math.round(edge.score * 100) + '%' : ''} semantically similar ` +
        `to the opened topic (embedding cosine; 45% minimum to appear).`
      )
    case 'THREAD':
      return 'A message in the same Slack thread as the opened topic.'
    default:
      return `Explicitly linked: a "${edge.kind.toLowerCase()}" reference was found between the two.`
  }
}

interface NeighborGroup {
  label: string
  order: number
  items: GraphNeighbor[]
}

// Group neighbors by friendly type label. Slack messages sharing a thread collapse
// to one row (the thread summary, dated by the thread's latest message). Groups
// sort Jira/PRs/Docs/Slack first; items within a group sort by time, newest first.
function groupNeighbors(neighbors: GraphNeighbor[]): NeighborGroup[] {
  // A collapsed thread sorts by its most recent message, so first compute the max
  // ts per thread across all its messages.
  const threadMax = new Map<string, number>()
  for (const n of neighbors) {
    const tt = n.node.thread_ts
    if (tt) threadMax.set(tt, Math.max(threadMax.get(tt) ?? 0, n.node.ts_ms ?? 0))
  }
  const effTs = (n: GraphNeighbor): number =>
    n.node.last_ts_ms || (n.node.thread_ts && threadMax.get(n.node.thread_ts)) || n.node.ts_ms || 0

  const byLabel = new Map<string, NeighborGroup>()
  const seenThread = new Set<string>()
  for (const n of neighbors) {
    // Collapse a Slack thread's messages into a single row.
    const tt = n.node.thread_ts
    if ((n.node.type === 'slack' || n.node.type === 'slack_thread') && tt) {
      if (seenThread.has(tt)) continue
      seenThread.add(tt)
    }
    const cfg = GRAPH_TYPE_GROUPS[n.node.type]
    const label = cfg?.label ?? n.node.type
    const order = cfg?.order ?? 99
    let g = byLabel.get(label)
    if (!g) {
      g = { label, order, items: [] }
      byLabel.set(label, g)
    }
    g.items.push(n)
  }
  for (const g of byLabel.values()) {
    // Highest similarity first (a 84% match outranks a 77% one); explicit edges
    // have no score and fall back to newest-first among themselves.
    g.items.sort(
      (a, b) => (b.edge.score ?? 0) - (a.edge.score ?? 0) || effTs(b) - effTs(a),
    )
  }
  return [...byLabel.values()].sort((a, b) => a.order - b.order || a.label.localeCompare(b.label))
}

// ── Horizontal neighbor timeline ────────────────────────────────────────────
// One dot per row shown in the lists below (threads collapsed the same way),
// placed on a left→right time axis. Labels and tooltips include HH:MM.

function fmtDateHM(ms: number): string {
  const d = new Date(ms)
  return (
    d.toLocaleDateString('en-US', { year: 'numeric', month: 'short', day: 'numeric' }) +
    ' ' +
    d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })
  )
}

function typeDotColor(type: string): string {
  if (type === 'jira' || type === 'jira_attachment') return C.amber
  if (type === 'gh_pr') return C.green
  if (type.startsWith('cf') || type.startsWith('gws') || type === 'wegohub' || type === 'claude_artifact')
    return '#66aaff'
  if (type === 'person') return '#cc88ff'
  return C.text // slack threads + anything else
}

function NeighborTimeline({
  neighbors,
  numbers,
}: {
  neighbors: GraphNeighbor[]
  numbers: Map<string, number>
}) {
  // Mirror groupNeighbors' thread collapse: one point per Slack thread, dated
  // by its latest message.
  const threadMax = new Map<string, number>()
  for (const n of neighbors) {
    const tt = n.node.thread_ts
    if (tt) threadMax.set(tt, Math.max(threadMax.get(tt) ?? 0, n.node.ts_ms ?? 0))
  }
  const seen = new Set<string>()
  const pts: { n: GraphNeighbor; ts: number }[] = []
  for (const n of neighbors) {
    const tt = n.node.thread_ts
    if ((n.node.type === 'slack' || n.node.type === 'slack_thread') && tt) {
      if (seen.has(tt)) continue
      seen.add(tt)
    }
    if (n.node.type === 'person') continue // people aren't events in time
    const ts = n.node.last_ts_ms || (tt && threadMax.get(tt)) || n.node.ts_ms || 0
    if (ts > 0) pts.push({ n, ts })
  }
  if (pts.length < 2) return null
  pts.sort((a, b) => a.ts - b.ts)
  const min = pts[0].ts
  const max = pts[pts.length - 1].ts
  const span = Math.max(max - min, 1)

  // Scatter layout: x = time, y = similarity. Pixel-space so the strip can
  // scroll: the inner width grows with point count, and time labels cluster
  // by pixel distance (< LABEL_W) — every dot group gets a label, including
  // the right edge.
  const PAD_X = 12
  const LABEL_W = 160
  const CHART_H = 72
  const PAD_TOP = 10
  const PAD_BOT = 12
  const innerW = Math.max(560, pts.length * 170)
  const xOf = (ts: number) => PAD_X + ((ts - min) / span) * (innerW - 2 * PAD_X)

  const scores = pts.map((p) => p.n.edge.score ?? 0).filter((s) => s > 0)
  const smin = scores.length ? Math.min(...scores) : 0
  const smax = scores.length ? Math.max(...scores) : 0
  const yOf = (score: number) => {
    if (score <= 0) return CHART_H - PAD_BOT // unscored (explicit edges) sit on the baseline
    if (smax === smin) return CHART_H / 2
    return PAD_TOP + ((smax - score) / (smax - smin)) * (CHART_H - PAD_TOP - PAD_BOT)
  }

  const clusters: { x: number; ts: number; count: number }[] = []
  for (const p of pts) {
    const x = xOf(p.ts)
    const last = clusters[clusters.length - 1]
    if (last && x - last.x < LABEL_W) {
      last.count++
      continue
    }
    clusters.push({ x, ts: p.ts, count: 1 })
  }

  return (
    <div>
      <div
        style={{
          display: 'flex',
          alignItems: 'baseline',
          gap: 6,
          color: C.dim,
          fontSize: 10,
          letterSpacing: '0.1em',
          textTransform: 'uppercase',
        }}
      >
        <span>Timeline</span>
        <span style={{ opacity: 0.7 }}>· {pts.length}</span>
      </div>
      <div style={{ display: 'flex', gap: 6, marginTop: 4 }}>
        {/* y-axis: similarity, outside the scroll so it stays visible */}
        {smax > 0 && (
          <div
            style={{
              display: 'flex',
              flexDirection: 'column',
              justifyContent: 'space-between',
              height: CHART_H,
              flexShrink: 0,
              color: C.dim,
              fontSize: 8,
              textAlign: 'right',
              width: 26,
            }}
          >
            <span>{Math.round(smax * 100)}%</span>
            <span>{smax === smin ? '' : `${Math.round(smin * 100)}%`}</span>
          </div>
        )}
        <div style={{ overflowX: 'auto', flex: 1, paddingBottom: 2 }}>
          <div style={{ width: innerW }}>
            <div style={{ position: 'relative', height: CHART_H }}>
              <div
                style={{
                  position: 'absolute',
                  left: 0,
                  right: 0,
                  top: CHART_H - PAD_BOT,
                  height: 1,
                  background: C.border,
                }}
              />
              {pts.map(({ n, ts }) => {
                const label = n.node.title || n.node.node_id.replace(/^[a-z_]+:/, '')
                const score = n.edge.score ?? 0
                const no = numbers.get(n.node.node_id)
                const pct = score > 0 ? ` · ${Math.round(score * 100)}%` : ''
                return (
                  <a
                    key={n.node.node_id}
                    href={n.node.url || undefined}
                    target="_blank"
                    rel="noopener noreferrer"
                    title={`${fmtDateHM(ts)}${pct} — ${label}`}
                    style={{
                      position: 'absolute',
                      left: xOf(ts) - 6.5,
                      top: yOf(score) - 6.5,
                      width: 13,
                      height: 13,
                      borderRadius: '50%',
                      background: typeDotColor(n.node.type),
                      opacity: 0.9,
                      cursor: n.node.url ? 'pointer' : 'help',
                      display: 'flex',
                      alignItems: 'center',
                      justifyContent: 'center',
                      fontSize: 8,
                      fontWeight: 600,
                      color: '#0a0a0a',
                      textDecoration: 'none',
                    }}
                  >
                    {no ?? ''}
                  </a>
                )
              })}
            </div>
            <div style={{ position: 'relative', height: 12, marginTop: 2 }}>
              {clusters.map((c) => (
                <span
                  key={c.ts}
                  style={{
                    position: 'absolute',
                    left: c.x,
                    transform:
                      c.x < 70 ? 'none' : c.x > innerW - 70 ? 'translateX(-100%)' : 'translateX(-50%)',
                    color: C.dim,
                    fontSize: 8,
                    letterSpacing: '0.04em',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {fmtDateHM(c.ts)}
                  {c.count > 1 ? ` ·${c.count}` : ''}
                </span>
              ))}
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

const REFRESH_OPTIONS = [5, 10, 30] as const
const WINDOW_OPTIONS = [
  { days: 90, label: '3 MONTHS' },
  { days: 365, label: 'THIS YEAR' },
  { days: 0, label: 'ALL' },
] as const
const PULSE_MS = 2000
// Recent-activity ticker: server-backed (top channels by new messages); the
// client drops rows older than this window between polls so a stalled poll
// doesn't leave stale entries on screen.
const ACTIVITY_WINDOW_MS = 30 * 60 * 1000

// Viewer's local IANA timezone, shown so message times are unambiguous.
const localTz = Intl.DateTimeFormat().resolvedOptions().timeZone

// Marker radii in viewBox units (360×180 space). sqrt-scaled by count, clamped so
// tiny channels stay visible and the biggest doesn't dominate the map.
const MIN_R = 0.6
const MAX_R = 4.5

// equirectangular projection: viewBox is 0..360 x, 0..180 y.
function projX(lon: number): number {
  return lon + 180
}
function projY(lat: number): number {
  return 90 - lat
}

interface LivePoint {
  channelId: string
  name: string
  country: string
  count: number
  lat: number
  lng: number
  color: string
  continentId: string
  // Epoch ms until which this point is highlighted (its count just grew).
  pulseUntil: number
}

interface ActivityEntry {
  channelId: string
  country: string
  name: string
  delta: number
  at: number
}

type GeoGeometry =
  | { type: 'Polygon'; coordinates: number[][][] }
  | { type: 'MultiPolygon'; coordinates: number[][][][] }

interface GeoFeature {
  geometry: GeoGeometry | null
  properties: Record<string, unknown>
}

interface GeoJSON {
  features: GeoFeature[]
}

function buildPoints(
  channels: ChannelCount[],
  cfg: ContinentCfg,
  prevPulse: Map<string, number>,
): LivePoint[] {
  // Country assignment is deterministic given the same sorted channel input, so
  // a channel keeps its country within a window across polls.
  const assigned = assignCountries(channels, cfg)
  const colorById = new Map(cfg.continents.map((c) => [c.id, c.color]))
  const pts: LivePoint[] = []
  for (const ch of channels) {
    const country = assigned[ch.channel_id]
    if (!country) continue // ran out of countries (won't happen for <168)
    const cid = continentOf(ch.channel_id, cfg, ch.name)
    pts.push({
      channelId: ch.channel_id,
      name: nameOf(ch.channel_id, cfg, ch.name),
      country: country.name,
      count: ch.count,
      lat: country.lat,
      lng: country.lon,
      color: colorById.get(cid) ?? '#8b949e',
      continentId: cid,
      pulseUntil: prevPulse.get(ch.channel_id) ?? 0,
    })
  }
  return pts
}

// Convert one polygon (array of linear rings) into an SVG path string.
function ringsToPath(rings: number[][][]): string {
  let d = ''
  for (const ring of rings) {
    if (ring.length === 0) continue
    for (let i = 0; i < ring.length; i++) {
      const [lon, lat] = ring[i]
      const x = projX(lon)
      const y = projY(lat)
      d += `${i === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`
    }
    d += 'Z'
  }
  return d
}

// Build SVG path strings for every country feature.
function geometriesToPaths(geo: GeoJSON): string[] {
  const paths: string[] = []
  for (const f of geo.features) {
    const g = f.geometry
    if (!g) continue
    if (g.type === 'Polygon') {
      const d = ringsToPath(g.coordinates)
      if (d) paths.push(d)
    } else if (g.type === 'MultiPolygon') {
      for (const poly of g.coordinates) {
        const d = ringsToPath(poly)
        if (d) paths.push(d)
      }
    }
  }
  return paths
}

export function LiveGlobePage() {
  const [cfg, setCfg] = useState<ContinentCfg | null>(null)
  const [points, setPoints] = useState<LivePoint[]>([])
  const [countryPaths, setCountryPaths] = useState<string[]>([])
  const [hidden, setHidden] = useState<Set<string>>(new Set())
  const [interval, setIntervalSec] = useState<number>(10)
  const [windowDays, setWindowDays] = useState<number>(90)
  const [lastUpdate, setLastUpdate] = useState<number>(0)
  const [tick, setTick] = useState(0)
  const [error, setError] = useState('')
  const [activity, setActivity] = useState<ActivityEntry[]>([])
  const [hovered, setHovered] = useState<string | null>(null)
  const [selected, setSelected] = useState<LivePoint | null>(null)
  const [topics, setTopics] = useState<ChannelTopic[]>([])
  const [topicsLoading, setTopicsLoading] = useState(false)
  // Baseline (epoch ms) captured on open: topics with last_ms > this are NEW.
  const [lastSeen, setLastSeen] = useState(0)
  // Which thread rows are expanded, and the lazily-fetched messages per thread.
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [threadMsgs, setThreadMsgs] = useState<Record<string, ChannelMessage[]>>({})
  const [threadLoading, setThreadLoading] = useState<Set<string>>(new Set())
  // Guards the topics fetch so a refresh tick can't overlap an in-flight load.
  const topicsInFlightRef = useRef(false)

  // ── "Open in Graph" overlay: shows a thread's related cross-source resources ──
  // graphTopic is the topic whose neighbors are shown; null = overlay closed.
  const [graphTopic, setGraphTopic] = useState<ChannelTopic | null>(null)
  const [graphLoading, setGraphLoading] = useState(false)
  // Cache neighbors per node_id so reopening the same node is instant.
  const [neighborCache, setNeighborCache] = useState<Record<string, GraphNeighbor[]>>({})
  // Drill stack: each entry is one "root" the overlay is currently showing.
  // The last entry is the active root; earlier entries are breadcrumbs. Drilling
  // into a neighbor pushes it here, so we walk the graph one hop at a time
  // instead of fetching an ever-deeper (and exponentially larger) single query.
  const [graphStack, setGraphStack] = useState<{ id: string; label: string }[]>([])
  // LLM cluster synthesis per node_id (what this is + what happened on Slack).
  const [summaryCache, setSummaryCache] = useState<Record<string, ClusterSummary | 'loading'>>({})
  // Overlay view: the readable synthesis, or the visual node-link diagram.
  const [graphView, setGraphView] = useState<'summary' | 'diagram'>('summary')

  // ── Graph search (ordered by score, then created_at desc — server-side) ───────
  const [searchQ, setSearchQ] = useState('')
  const [searchResults, setSearchResults] = useState<GraphNode[] | null>(null)
  const [searchLoading, setSearchLoading] = useState(false)

  function runSearch(q: string) {
    const term = q.trim()
    if (!term) {
      setSearchResults(null)
      return
    }
    setSearchLoading(true)
    graphSearch(term, undefined, 20)
      .then((r) => setSearchResults(r.results || []))
      .catch(() => setSearchResults([]))
      .finally(() => setSearchLoading(false))
  }

  // ── Topic subscriptions (enzobot hot-topic DM alerts) ─────────────────────────
  const [subsOpen, setSubsOpen] = useState(false)
  const [subs, setSubs] = useState<TopicSubscription[]>([])
  const [subTopic, setSubTopic] = useState('')
  const [subChannel, setSubChannel] = useState('') // optional: limit to one channel
  const [subSources, setSubSources] = useState<TopicSource[]>([]) // dynamic knowledge sources
  const [subBusy, setSubBusy] = useState(false)
  const [subError, setSubError] = useState('')
  const [editingSub, setEditingSub] = useState<number | null>(null) // card whose sources are being edited
  const [editSources, setEditSources] = useState<TopicSource[]>([])

  function refreshSubs() {
    listSubscriptions()
      .then((s) => setSubs(s || []))
      .catch(() => setSubs([]))
  }

  // ── Dynamic source rows (type dropdown + URL, add/remove) ────────────────────
  function addSourceRow() {
    setSubSources((cur) => [...cur, { type: 'confluence', url: '' }])
  }
  function updateSource(i: number, patch: Partial<TopicSource>) {
    setSubSources((cur) => cur.map((s, idx) => (idx === i ? { ...s, ...patch } : s)))
  }
  function removeSource(i: number) {
    setSubSources((cur) => cur.filter((_, idx) => idx !== i))
  }
  // buildSources returns the non-empty source rows for submission.
  function buildSources(): TopicSource[] {
    return subSources
      .filter((s) => s.url.trim())
      .map((s) => ({ type: s.type, url: s.url.trim() }))
  }

  // ── Edit an existing subscription's sources ──────────────────────────────────
  function startEdit(s: TopicSubscription) {
    setEditingSub(s.id)
    setEditSources((s.sources ?? []).map((x) => ({ ...x })))
    setSubError('')
  }
  function editAddRow() {
    setEditSources((cur) => [...cur, { type: 'confluence', url: '' }])
  }
  function editUpdate(i: number, patch: Partial<TopicSource>) {
    setEditSources((cur) => cur.map((s, idx) => (idx === i ? { ...s, ...patch } : s)))
  }
  function editRemove(i: number) {
    setEditSources((cur) => cur.filter((_, idx) => idx !== i))
  }
  function saveEdit(id: number) {
    const sources = editSources.filter((s) => s.url.trim()).map((s) => ({ type: s.type, url: s.url.trim() }))
    setSubBusy(true)
    setSubError('')
    updateSubscription(id, { sources })
      .then(() => {
        setEditingSub(null)
        refreshSubScope(id) // re-read + re-distill the scope from the new source set
      })
      .catch((e: unknown) => setSubError(e instanceof Error ? e.message : 'update failed'))
      .finally(() => setSubBusy(false))
  }

  function addSub() {
    const topic = subTopic.trim()
    if (!topic) return
    setSubBusy(true)
    setSubError('')
    const sources = buildSources()
    createSubscription({
      topic,
      channel_filter: subChannel.trim() ? [subChannel.trim()] : undefined,
      sources: sources.length ? sources : undefined,
    })
      .then((created) => {
        setSubTopic('')
        setSubChannel('')
        setSubSources([])
        // If sources were given, kick off the read/analyze so the scope summary fills in.
        if (sources.length && created?.id) {
          refreshSubScope(created.id)
        } else {
          refreshSubs()
        }
      })
      .catch((e: unknown) => setSubError(e instanceof Error ? e.message : 'failed to subscribe'))
      .finally(() => setSubBusy(false))
  }

  // refreshSubScope triggers a source re-read and polls until the scope is ready.
  function refreshSubScope(id: number) {
    refreshSubscription(id)
      .then(() => {
        refreshSubs()
        let tries = 0
        const poll = window.setInterval(() => {
          tries++
          listSubscriptions()
            .then((list) => {
              setSubs(list || [])
              const me = (list || []).find((x) => x.id === id)
              if (!me || me.scope_status !== 'refreshing' || tries > 40) {
                window.clearInterval(poll)
              }
            })
            .catch(() => {})
        }, 5000)
      })
      .catch((e: unknown) => setSubError(e instanceof Error ? e.message : 'refresh failed'))
  }

  function removeSub(id: number) {
    deleteSubscription(id)
      .then(refreshSubs)
      .catch(() => {})
  }

  useEffect(() => {
    if (subsOpen) refreshSubs()
  }, [subsOpen])

  // Open the existing "Graph" overlay for a search hit by adapting it to a topic.
  function openGraphForNode(n: GraphNode) {
    openGraph({
      thread_ts: '',
      node_id: n.id,
      summary: n.title || n.summary || n.id,
      is_thread: false,
      msg_count: 0,
      participants: [],
      first_ms: 0,
      last_ms: 0,
      url: n.url || '',
    })
  }

  // ── Zoom + pan transform (viewBox space: 360×180) ────────────────────────────
  const [view, setView] = useState({ k: 1, tx: 0, ty: 0 })
  const svgRef = useRef<SVGSVGElement | null>(null)
  // Drag tracking: start pointer + start translate, plus whether we moved enough
  // to count as a pan (so a plain click still opens the data panel).
  const dragRef = useRef<{
    pointerId: number
    startX: number
    startY: number
    startTx: number
    startTy: number
    moved: boolean
  } | null>(null)
  // Set true on pointerup when the gesture was a pan, so the click that fires
  // immediately afterwards is swallowed (clicks fire after pointerup).
  const suppressClickRef = useRef(false)

  // Refs mirror state for use inside the polling closure without re-subscribing.
  const prevCountsRef = useRef<Map<string, number>>(new Map())
  const cfgRef = useRef<ContinentCfg | null>(null)
  const inFlightRef = useRef(false)
  const windowDaysRef = useRef(90)
  windowDaysRef.current = windowDays

  // ── Load the basemap geojson once (same-origin static asset) ─────────────────
  useEffect(() => {
    let cancelled = false
    fetch('/data/countries.geojson')
      .then((r) => r.json())
      .then((geo: GeoJSON) => {
        if (!cancelled) setCountryPaths(geometriesToPaths(geo))
      })
      .catch(() => {
        /* basemap is decorative; markers still render without it */
      })
    return () => {
      cancelled = true
    }
  }, [])

  // ── Channel poll (guarded against overlap, re-created on interval/window) ─────
  useEffect(() => {
    let cancelled = false

    async function poll() {
      if (cancelled || inFlightRef.current || !cfgRef.current) return
      inFlightRef.current = true
      try {
        const channels = await fetchChannels(windowDaysRef.current)
        if (cancelled) return
        const cfgNow = cfgRef.current
        if (!cfgNow) return

        const prev = prevCountsRef.current
        const now = Date.now()
        const pulse = new Map<string, number>()
        const assigned = assignCountries(channels || [], cfgNow)
        // Diff poll-to-poll counts to pulse markers that just grew.
        for (const ch of channels || []) {
          const before = prev.get(ch.channel_id)
          if (before !== undefined && ch.count > before) {
            pulse.set(ch.channel_id, now + PULSE_MS)
          }
        }
        // Record current counts for the next diff.
        const nextCounts = new Map<string, number>()
        for (const ch of channels || []) nextCounts.set(ch.channel_id, ch.count)
        prevCountsRef.current = nextCounts

        setPoints(buildPoints(channels || [], cfgNow, pulse))
        setLastUpdate(now)
        setError('')

        // Recent-activity ticker is server-backed: top channels by new messages
        // in the window, shared across viewers and populated on first load.
        const recent = await fetchRecentActivity()
        if (!cancelled) {
          setActivity(
            recent.map((rc) => ({
              channelId: rc.channel_id,
              country: assigned[rc.channel_id]?.name ?? '',
              name: nameOf(rc.channel_id, cfgNow, rc.name),
              delta: rc.delta,
              at: rc.at_ms,
            })),
          )
        }
      } catch (err: unknown) {
        if (!cancelled) setError(err instanceof Error ? err.message : 'Failed to load channels')
      } finally {
        inFlightRef.current = false
      }
    }

    // Reset the diff baseline when the window changes (counts aren't comparable).
    prevCountsRef.current = new Map()

    // Load continents config first, then begin polling.
    fetchContinents()
      .then((c) => {
        if (cancelled) return
        cfgRef.current = c
        setCfg(c)
        void poll()
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : 'Failed to load config')
      })

    const id = window.setInterval(() => {
      if (document.hidden) return
      void poll()
    }, interval * 1000)

    // Refresh continents config periodically so config edits show up.
    const cfgId = window.setInterval(() => {
      if (document.hidden) return
      fetchContinents()
        .then((c) => {
          if (!cancelled) {
            cfgRef.current = c
            setCfg(c)
          }
        })
        .catch(() => {})
    }, 60000)

    // Resume + immediately refetch when the tab becomes visible again.
    function onVisibility() {
      if (!document.hidden) void poll()
    }
    document.addEventListener('visibilitychange', onVisibility)

    return () => {
      cancelled = true
      window.clearInterval(id)
      window.clearInterval(cfgId)
      document.removeEventListener('visibilitychange', onVisibility)
    }
    // Re-run when interval or window changes so polling uses the new settings.
  }, [interval, windowDays])

  // ── Ticker: drives "updated Xs ago" + decays pulses ─────────────────────────
  useEffect(() => {
    const id = window.setInterval(() => setTick((t) => t + 1), 1000)
    return () => window.clearInterval(id)
  }, [])

  // ── Click panel: load TOPIC summaries for the selected channel + window ──────
  // On open: capture the per-channel "last seen" baseline from localStorage (used
  // to mark NEW topics), then fetch topics. On window change / refresh tick we
  // re-fetch the same channel's topics (guarded against overlap). On close /
  // channel switch / unmount we stamp localStorage so nothing stays NEW next open.
  const selectedIdRef = useRef<string | null>(null)
  useEffect(() => {
    if (!selected) {
      selectedIdRef.current = null
      setTopics([])
      setExpanded(new Set())
      setThreadMsgs({})
      setThreadLoading(new Set())
      return
    }
    const channelId = selected.channelId
    selectedIdRef.current = channelId
    // Capture the baseline BEFORE rendering so NEW reflects the prior visit.
    const raw = localStorage.getItem('liveSeen:' + channelId)
    setLastSeen(raw ? Number(raw) || 0 : 0)
    // Reset expand state when switching channels.
    setExpanded(new Set())
    setThreadMsgs({})
    setThreadLoading(new Set())

    let cancelled = false
    async function loadTopics(showLoading: boolean) {
      if (cancelled || topicsInFlightRef.current) return
      topicsInFlightRef.current = true
      if (showLoading) setTopicsLoading(true)
      try {
        const t = await fetchChannelTopics(channelId, windowDays)
        if (!cancelled) setTopics(t || [])
      } catch {
        if (!cancelled && showLoading) setTopics([])
      } finally {
        topicsInFlightRef.current = false
        if (!cancelled && showLoading) setTopicsLoading(false)
      }
    }
    void loadTopics(true)
    // Keep NEW/topics live: re-fetch on the same cadence as the marker poll.
    const id = window.setInterval(() => {
      if (document.hidden) return
      void loadTopics(false)
    }, interval * 1000)

    return () => {
      cancelled = true
      window.clearInterval(id)
      // Stamp "seen now" so these topics are no longer NEW on the next open.
      try {
        localStorage.setItem('liveSeen:' + channelId, String(Date.now()))
      } catch {
        /* ignore quota / disabled storage */
      }
    }
  }, [selected, windowDays, interval])

  // ── Lazy-load a thread's messages when its row is expanded ───────────────────
  function toggleTopic(t: ChannelTopic) {
    if (!t.is_thread) return // single messages aren't expandable
    const key = t.thread_ts
    setExpanded((cur) => {
      const next = new Set(cur)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
    // Fetch once (cache per thread); skip if already loaded or loading.
    if (threadMsgs[key] || threadLoading.has(key) || !selected) return
    const channelId = selected.channelId
    setThreadLoading((cur) => new Set(cur).add(key))
    fetchChannelMessages(channelId, windowDays, 100, key)
      .then((m) => setThreadMsgs((cur) => ({ ...cur, [key]: m || [] })))
      .catch(() => setThreadMsgs((cur) => ({ ...cur, [key]: [] })))
      .finally(() =>
        setThreadLoading((cur) => {
          const next = new Set(cur)
          next.delete(key)
          return next
        }),
      )
  }

  // ── Open the "related resources" graph overlay for a topic ───────────────────
  // loadNeighbors fetches a node's neighbors into the cache (depth 1 — drilling
  // gives the user explicit control over how deep to go).
  function loadNeighbors(nodeId: string, depth = 1) {
    if (neighborCache[nodeId]) return
    setGraphLoading(true)
    fetchNeighbors(nodeId, depth)
      .then((ns) => setNeighborCache((cur) => ({ ...cur, [nodeId]: ns || [] })))
      .catch(() => setNeighborCache((cur) => ({ ...cur, [nodeId]: [] })))
      .finally(() => setGraphLoading(false))
  }

  // loadSummary fetches the LLM cluster synthesis for a node (cached per session).
  function loadSummary(nodeId: string, depth = 2) {
    if (summaryCache[nodeId]) return
    setSummaryCache((cur) => ({ ...cur, [nodeId]: 'loading' }))
    fetchClusterSummary(nodeId, depth)
      .then((s) => setSummaryCache((cur) => ({ ...cur, [nodeId]: s })))
      .catch(() =>
        setSummaryCache((cur) => ({
          ...cur,
          [nodeId]: { overview: '', highlights: [], nodes: [], edges: [], node_count: 0 },
        })),
      )
  }

  function openGraph(t: ChannelTopic) {
    if (!t.node_id) return
    setGraphTopic(t)
    setGraphStack([{ id: t.node_id, label: cfg ? applyGroupNames(t.summary, cfg) : t.summary }])
    loadNeighbors(t.node_id, 2)
    loadSummary(t.node_id, 2)
  }

  // drillInto re-roots the overlay at a neighbor, loading its own links — this is
  // the "load more" path: walk as deep as you want, one controlled hop at a time.
  function drillInto(nodeId: string, label: string) {
    setGraphStack((cur) => [...cur, { id: nodeId, label }])
    loadNeighbors(nodeId)
    loadSummary(nodeId, 2)
  }

  function graphBack() {
    setGraphStack((cur) => (cur.length > 1 ? cur.slice(0, -1) : cur))
  }

  // ── Derived control-panel data ──────────────────────────────────────────────
  const legend = useMemo(() => {
    if (!cfg) return []
    return cfg.continents.map((c) => {
      const cpts = points.filter((p) => p.continentId === c.id)
      return {
        id: c.id,
        label: c.label,
        color: c.color,
        channelCount: cpts.length,
        msgs: cpts.reduce((s, p) => s + p.count, 0),
      }
    })
  }, [cfg, points])

  // Recent-activity entries still inside the rolling window (drops stale rows on
  // each tick). `now` must be declared before this filter — it runs synchronously
  // during render, so a later `const now` would hit the temporal dead zone and
  // throw once `activity` is non-empty (white-paged the whole page).
  const now = Date.now()
  const recentActivity = activity.filter((a) => now - a.at < ACTIVITY_WINDOW_MS)

  const visiblePts = points.filter((p) => !hidden.has(p.continentId))
  const maxCount = Math.max(1, ...visiblePts.map((p) => p.count))
  const totalChannels = visiblePts.length
  const totalMsgs = visiblePts.reduce((s, p) => s + p.count, 0)
  const secsAgo = lastUpdate ? Math.max(0, Math.round((Date.now() - lastUpdate) / 1000)) : null
  const windowLabel = WINDOW_OPTIONS.find((w) => w.days === windowDays)?.label ?? 'ALL'
  void tick // keeps secsAgo recomputing + pulses decaying every second

  // Continent label for the selected channel's panel.
  const selectedContinent = useMemo(() => {
    if (!selected || !cfg) return null
    return cfg.continents.find((c) => c.id === selected.continentId) ?? null
  }, [selected, cfg])

  function toggleContinent(id: string) {
    setHidden((cur) => {
      const next = new Set(cur)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  function radiusFor(count: number): number {
    const norm = Math.sqrt(count) / Math.sqrt(maxCount)
    return MIN_R + (MAX_R - MIN_R) * norm
  }

  // ── Zoom + pan ──────────────────────────────────────────────────────────────
  const K_MIN = 1
  const K_MAX = 8

  // Map a client-space point (px) to viewBox space (0..360 × 0..180).
  function clientToViewBox(clientX: number, clientY: number): { x: number; y: number } {
    const svg = svgRef.current
    if (!svg) return { x: 0, y: 0 }
    const rect = svg.getBoundingClientRect()
    // preserveAspectRatio="xMidYMid meet": the 360×180 (2:1) viewBox is letterboxed
    // inside the element. Compute the rendered map rect and map into it.
    const scale = Math.min(rect.width / 360, rect.height / 180)
    const drawW = 360 * scale
    const drawH = 180 * scale
    const offX = (rect.width - drawW) / 2
    const offY = (rect.height - drawH) / 2
    const x = (clientX - rect.left - offX) / scale
    const y = (clientY - rect.top - offY) / scale
    return { x, y }
  }

  function onWheel(e: React.WheelEvent<SVGSVGElement>) {
    e.preventDefault()
    setView((v) => {
      const factor = Math.exp(-e.deltaY * 0.0015)
      const nextK = Math.min(K_MAX, Math.max(K_MIN, v.k * factor))
      if (nextK === v.k) return v
      // Keep the point under the cursor fixed: solve for tx/ty so that the
      // svg-space point p stays at the same screen location across the k change.
      const p = clientToViewBox(e.clientX, e.clientY)
      const tx = p.x - (p.x - v.tx) * (nextK / v.k)
      const ty = p.y - (p.y - v.ty) * (nextK / v.k)
      return { k: nextK, tx, ty }
    })
  }

  function onPointerDown(e: React.PointerEvent<SVGSVGElement>) {
    // Only primary button initiates a pan.
    if (e.button !== 0) return
    dragRef.current = {
      pointerId: e.pointerId,
      startX: e.clientX,
      startY: e.clientY,
      startTx: view.tx,
      startTy: view.ty,
      moved: false,
    }
    // NOTE: do NOT setPointerCapture here — capturing on the <svg> would make the
    // browser fire `click` on the svg instead of the child <circle>, so markers
    // would never receive clicks. We capture only once a real drag starts.
  }

  function onPointerMove(e: React.PointerEvent<SVGSVGElement>) {
    const d = dragRef.current
    if (!d || d.pointerId !== e.pointerId) return
    const svg = svgRef.current
    if (!svg) return
    const rect = svg.getBoundingClientRect()
    const scale = Math.min(rect.width / 360, rect.height / 180)
    if (!d.moved && Math.hypot(e.clientX - d.startX, e.clientY - d.startY) > 4) {
      d.moved = true
      // Now it's a real pan — capture so panning continues smoothly off-target.
      try {
        e.currentTarget.setPointerCapture(e.pointerId)
      } catch {
        /* ignore */
      }
    }
    if (!d.moved) return // a plain click: don't pan, leave the marker click intact
    // Convert client-pixel delta to viewBox-unit delta (independent of k:
    // tx/ty are pre-scale translation in the group transform).
    const dx = (e.clientX - d.startX) / scale
    const dy = (e.clientY - d.startY) / scale
    setView((v) => ({ ...v, tx: d.startTx + dx, ty: d.startTy + dy }))
  }

  function onPointerUp(e: React.PointerEvent<SVGSVGElement>) {
    const d = dragRef.current
    if (!d || d.pointerId !== e.pointerId) return
    // Remember whether this gesture panned, so the upcoming click is swallowed.
    suppressClickRef.current = d.moved
    dragRef.current = null
    if (e.currentTarget.hasPointerCapture(e.pointerId)) {
      e.currentTarget.releasePointerCapture(e.pointerId)
    }
  }

  function zoomBy(factor: number) {
    setView((v) => {
      const nextK = Math.min(K_MAX, Math.max(K_MIN, v.k * factor))
      if (nextK === v.k) return v
      // Zoom toward the map center (viewBox 180,90).
      const cx = 180
      const cy = 90
      const tx = cx - (cx - v.tx) * (nextK / v.k)
      const ty = cy - (cy - v.ty) * (nextK / v.k)
      return { k: nextK, tx, ty }
    })
  }

  function resetView() {
    setView({ k: 1, tx: 0, ty: 0 })
  }

  // Center the globe on a channel's marker and open its panel (used by the
  // recent-activity ticker). No-op if the channel isn't currently on the map.
  function focusChannel(channelId: string) {
    const p = points.find((pt) => pt.channelId === channelId)
    if (!p) return
    setSelected(p)
    const FOCUS_K = 4
    // translate(tx ty) scale(k): solve tx/ty so the marker lands at viewBox center.
    setView({ k: FOCUS_K, tx: 180 - FOCUS_K * projX(p.lng), ty: 90 - FOCUS_K * projY(p.lat) })
  }

  const k = view.k

  // Label de-collision: in dense clusters two channel labels can overdraw each
  // other (one name rendered on top of another). Greedily keep labels for the
  // highest-priority markers — hovered/selected, then busiest — and suppress any
  // label whose box would overlap one already placed. The dot always stays, and
  // hovering a suppressed marker brings its label back (hover wins priority).
  // All maths in viewBox units, mirroring the per-marker label layout below.
  const labelVisible = (() => {
    type Box = { x: number; y: number; w: number; h: number }
    const hit = (a: Box, b: Box) =>
      a.x < b.x + b.w && a.x + a.w > b.x && a.y < b.y + b.h && a.y + a.h > b.y
    const fontPrimary = 2 / Math.pow(k, 0.6)
    const prio = (p: (typeof visiblePts)[number]) =>
      (p.channelId === hovered ? 2 : 0) + (selected?.channelId === p.channelId ? 1 : 0)
    const ordered = [...visiblePts].sort((a, b) => prio(b) - prio(a) || b.count - a.count)
    const placed: Box[] = []
    const shown = new Set<string>()
    for (const p of ordered) {
      const cx = projX(p.lng)
      const cy = projY(p.lat)
      const r = radiusFor(p.count) / k
      const hasName = p.name !== p.channelId
      const maxChars = Math.max(p.name.length, hasName ? p.channelId.length : 0)
      const box: Box = {
        x: cx + r + 1 / k,
        y: cy - fontPrimary * 0.9,
        w: maxChars * fontPrimary * 0.62,
        h: fontPrimary * (hasName ? 2.4 : 1.4),
      }
      if (placed.some((q) => hit(box, q))) continue
      placed.push(box)
      shown.add(p.channelId)
    }
    return shown
  })()

  // ── Shared chrome styles ─────────────────────────────────────────────────────
  const panel: React.CSSProperties = {
    background: C.panel,
    border: `1px solid ${C.border}`,
    fontFamily: MONO,
    color: C.text,
  }
  const segBtn = (active: boolean): React.CSSProperties => ({
    fontFamily: MONO,
    fontSize: 10,
    letterSpacing: '0.06em',
    padding: '4px 8px',
    background: active ? 'rgba(68,255,136,0.14)' : 'transparent',
    color: active ? C.green : C.dim,
    border: `1px solid ${active ? C.green : C.border}`,
    borderRadius: 2,
    cursor: 'pointer',
    textTransform: 'uppercase',
  })

  return (
    <div style={{ position: 'fixed', inset: 0, width: '100vw', height: '100vh', overflow: 'hidden', background: C.bg }}>
      <style>{`
        @keyframes map-pulse {
          0%   { r: 0; opacity: 0.9; }
          100% { r: 9; opacity: 0; }
        }
        @keyframes live-blink {
          0%, 100% { opacity: 1; }
          50%      { opacity: 0.25; }
        }
      `}</style>

      {/* ── 2D equirectangular world map ──────────────────────────────────────── */}
      <svg
        ref={svgRef}
        viewBox="0 0 360 180"
        preserveAspectRatio="xMidYMid meet"
        onWheel={onWheel}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        style={{
          position: 'absolute',
          inset: 0,
          width: '100%',
          height: '100%',
          background: C.bg,
          cursor: dragRef.current?.moved ? 'grabbing' : 'grab',
          touchAction: 'none',
        }}
      >
        <defs>
          <filter id="marker-glow" x="-200%" y="-200%" width="500%" height="500%">
            <feGaussianBlur stdDeviation="1.2" result="blur" />
            <feMerge>
              <feMergeNode in="blur" />
              <feMergeNode in="SourceGraphic" />
            </feMerge>
          </filter>
        </defs>

        {/* Zoom + pan transform wraps all map content (countries + markers + labels) */}
        <g transform={`translate(${view.tx} ${view.ty}) scale(${k})`}>
        {/* Countries */}
        <g>
          {countryPaths.map((d, i) => (
            <path key={i} d={d} fill={C.land} stroke={C.landStroke} strokeWidth={0.15 / k} />
          ))}
        </g>

        {/* Channel markers (visible continents only) */}
        <g>
          {visiblePts.map((p) => {
            const cx = projX(p.lng)
            const cy = projY(p.lat)
            // Marker radius / strokes / label sizes are counter-scaled by k so
            // they stay roughly constant on-screen as the map zooms.
            const r = radiusFor(p.count) / k
            const isHover = hovered === p.channelId
            const isSel = selected?.channelId === p.channelId
            const pulsing = p.pulseUntil > now
            // Label: primary = channel name, secondary = raw id. If nameOf
            // returned the id itself (no name configured), show only the id.
            const labelX = cx + r + 1 / k
            // Soft counter-scale (k^0.6, not k): labels stay the same on-screen
            // size at min zoom but grow as you zoom in (~2.3x at K_MAX=8).
            const labelK = Math.pow(k, 0.6)
            const fontPrimary = 2 / labelK
            const fontSecondary = fontPrimary * 0.7
            const labelStroke = 0.4 / labelK
            const hasName = p.name !== p.channelId
            // Invisible hit area spanning the dot + label so the whole marker is
            // hoverable/clickable, not just the (often tiny) core dot. Monospace
            // glyphs are ~0.6em wide; size the box to the longest visible line.
            const maxChars = Math.max(p.name.length, hasName ? p.channelId.length : 0)
            const hitLeft = cx - r * 1.6
            const hitTop = cy - Math.max(r * 1.6, fontPrimary * 1.2)
            const hitW = labelX - hitLeft + maxChars * fontPrimary * 0.62 + 1 / k
            const hitH = Math.max(r * 3.2, fontPrimary * 3)
            return (
              <g
                key={p.channelId}
                style={{ cursor: 'pointer' }}
                onMouseEnter={() => setHovered(p.channelId)}
                onMouseLeave={() => setHovered((h) => (h === p.channelId ? null : h))}
                onClick={() => {
                  // A drag that crossed the pan threshold shouldn't open the panel.
                  if (suppressClickRef.current) {
                    suppressClickRef.current = false
                    return
                  }
                  setSelected(p)
                }}
              >
                {/* invisible hit target (dot + label) — fill="transparent" still
                    receives pointer events, unlike fill="none" */}
                <rect x={hitLeft} y={hitTop} width={hitW} height={hitH} fill="transparent" />
                {/* soft halo */}
                <circle cx={cx} cy={cy} r={r * 2.2} fill={p.color} opacity={isHover || isSel ? 0.22 : 0.12} />
                {/* expanding ring when count just grew */}
                {pulsing && (
                  <circle cx={cx} cy={cy} fill="none" stroke={p.color} strokeWidth={0.4 / k} opacity={0.8}>
                    <animate attributeName="r" from={r} to={r + 9 / k} dur="2s" repeatCount="indefinite" />
                    <animate attributeName="opacity" from="0.8" to="0" dur="2s" repeatCount="indefinite" />
                  </circle>
                )}
                {/* core dot */}
                <circle
                  cx={cx}
                  cy={cy}
                  r={isHover ? r * 1.25 : r}
                  fill={p.color}
                  opacity={0.85}
                  stroke={isSel ? C.text : 'none'}
                  strokeWidth={isSel ? 0.4 / k : 0}
                  filter="url(#marker-glow)"
                />
                {/* label: name on line 1, raw id on line 2 (id-only if unnamed).
                    Hidden when it would collide with a higher-priority label
                    (see labelVisible); the dot stays and hover brings it back. */}
                {labelVisible.has(p.channelId) && (
                  <text
                    x={labelX}
                    y={cy + 0.7 / k}
                    fontSize={fontPrimary}
                    fontFamily={MONO}
                    fill="#cbd5e1"
                    style={{ paintOrder: 'stroke', pointerEvents: 'none' }}
                    stroke="#0a0a0a"
                    strokeWidth={labelStroke}
                  >
                    {p.name}
                  </text>
                )}
                {labelVisible.has(p.channelId) && hasName && (
                  <text
                    x={labelX}
                    y={cy + 0.7 / k + fontPrimary}
                    fontSize={fontSecondary}
                    fontFamily={MONO}
                    fill="#6b7280"
                    style={{ paintOrder: 'stroke', pointerEvents: 'none' }}
                    stroke="#0a0a0a"
                    strokeWidth={labelStroke}
                  >
                    {p.channelId}
                  </text>
                )}
              </g>
            )
          })}
        </g>
        </g>
      </svg>

      {/* ── Top bar ──────────────────────────────────────────────────────────── */}
      <div
        style={{
          position: 'absolute',
          top: 0,
          left: 0,
          right: 0,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 12,
          padding: '8px 14px',
          background: 'linear-gradient(180deg, rgba(10,10,10,0.92), rgba(10,10,10,0))',
          fontFamily: MONO,
          flexWrap: 'wrap',
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <span
            style={{
              width: 8,
              height: 8,
              borderRadius: '50%',
              background: C.green,
              boxShadow: `0 0 8px ${C.green}`,
              animation: 'live-blink 1.4s ease-in-out infinite',
            }}
          />
          <span style={{ color: C.text, fontSize: 12, letterSpacing: '0.1em' }}>
            LIVE — WEGO AROUND ME
          </span>
          <a
            href="/"
            style={{
              marginLeft: 12,
              color: C.dim,
              fontSize: 10,
              letterSpacing: '0.08em',
              textDecoration: 'none',
            }}
          >
            ← DASHBOARD
          </a>
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              runSearch(searchQ)
            }}
            style={{ display: 'flex', alignItems: 'center', gap: 4 }}
          >
            <input
              value={searchQ}
              onChange={(e) => setSearchQ(e.target.value)}
              placeholder="SEARCH GRAPH…"
              style={{
                width: 200,
                background: C.panel,
                border: `1px solid ${C.border}`,
                color: C.text,
                fontFamily: MONO,
                fontSize: 10,
                letterSpacing: '0.06em',
                padding: '4px 8px',
                borderRadius: 2,
                outline: 'none',
              }}
            />
            {searchResults !== null && (
              <button
                type="button"
                onClick={() => {
                  setSearchQ('')
                  setSearchResults(null)
                }}
                style={segBtn(false)}
              >
                ✕
              </button>
            )}
          </form>
          <button type="button" onClick={() => setSubsOpen(true)} style={segBtn(subsOpen)}>
            🔔 ALERTS
          </button>
          <span style={{ color: C.dim, fontSize: 10, letterSpacing: '0.08em' }}>
            {secsAgo === null ? 'CONNECTING…' : `UPDATED ${secsAgo}S AGO`}
          </span>
          <div style={{ display: 'flex', gap: 4 }}>
            {WINDOW_OPTIONS.map((w) => (
              <button key={w.days} onClick={() => setWindowDays(w.days)} style={segBtn(windowDays === w.days)}>
                {w.label}
              </button>
            ))}
          </div>
          <div style={{ display: 'flex', gap: 4 }}>
            {REFRESH_OPTIONS.map((s) => (
              <button key={s} onClick={() => setIntervalSec(s)} style={segBtn(interval === s)}>
                {s}S
              </button>
            ))}
          </div>
        </div>
      </div>

      {error && (
        <div
          style={{
            position: 'absolute',
            top: 48,
            left: 14,
            color: C.red,
            fontFamily: MONO,
            fontSize: 11,
            background: 'rgba(10,10,10,0.85)',
            padding: '4px 8px',
            border: `1px solid ${C.red}`,
            borderRadius: 2,
          }}
        >
          {error}
        </div>
      )}

      {/* ── Search results panel (top-left, ordered by score then newest) ─────── */}
      {searchResults !== null && (
        <div
          style={{
            ...panel,
            position: 'absolute',
            top: 48,
            left: 14,
            width: 360,
            maxWidth: 'calc(100vw - 28px)',
            maxHeight: 'calc(100vh - 140px)',
            borderRadius: 3,
            padding: 12,
            display: 'flex',
            flexDirection: 'column',
            overflow: 'hidden',
            zIndex: 15,
          }}
        >
          <div
            style={{
              color: C.dim,
              fontSize: 9,
              letterSpacing: '0.12em',
              marginBottom: 8,
              borderBottom: `1px solid ${C.border}`,
              paddingBottom: 6,
            }}
          >
            SEARCH · {searchLoading ? 'SEARCHING…' : `${searchResults.length} RESULTS`}
          </div>
          <div style={{ flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: 6 }}>
            {!searchLoading && searchResults.length === 0 && (
              <div style={{ color: C.dim, fontSize: 11 }}>no matches</div>
            )}
            {searchResults.map((n) => {
              const label = n.title || n.summary || n.id
              const when = n.created_at
                ? new Date(n.created_at).toLocaleString(undefined, {
                    month: 'short',
                    day: 'numeric',
                    year: 'numeric',
                  })
                : ''
              const meta = [
                GRAPH_TYPE_GROUPS[n.type]?.label ?? n.type,
                n.author || '',
                when,
              ]
                .filter(Boolean)
                .join(' · ')
              return (
                <div
                  key={n.id}
                  style={{
                    background: 'rgba(255,255,255,0.02)',
                    border: `1px solid ${C.border}`,
                    borderRadius: 3,
                    padding: 8,
                    display: 'flex',
                    flexDirection: 'column',
                    gap: 4,
                  }}
                >
                  <div style={{ display: 'flex', alignItems: 'baseline', gap: 6 }}>
                    <span
                      style={{
                        flex: 1,
                        color: C.text,
                        fontSize: 11,
                        lineHeight: 1.35,
                        display: '-webkit-box',
                        WebkitLineClamp: 2,
                        WebkitBoxOrient: 'vertical',
                        overflow: 'hidden',
                      }}
                    >
                      {cfg ? applyGroupNames(label, cfg) : label}
                    </span>
                    {typeof n.score === 'number' && (
                      <span style={{ flexShrink: 0, color: C.green, fontSize: 9 }}>
                        {n.score.toFixed(2)}
                      </span>
                    )}
                  </div>
                  <div style={{ color: C.dim, fontSize: 9, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {meta}
                  </div>
                  <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
                    {n.url && (
                      <a
                        href={n.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        style={{ color: C.green, fontSize: 9, letterSpacing: '0.06em', textDecoration: 'none' }}
                      >
                        open ↗
                      </a>
                    )}
                    <button
                      onClick={() => openGraphForNode(n)}
                      style={{
                        background: 'transparent',
                        border: 'none',
                        padding: 0,
                        cursor: 'pointer',
                        fontFamily: MONO,
                        color: C.green,
                        fontSize: 9,
                        letterSpacing: '0.06em',
                      }}
                    >
                      open in Graph ↗
                    </button>
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      )}

      {/* ── Zoom controls (bottom-right, clear of legend + data panel) ───────── */}
      <div
        style={{
          position: 'absolute',
          bottom: 14,
          right: selected ? 366 : 244,
          display: 'flex',
          flexDirection: 'column',
          gap: 4,
          fontFamily: MONO,
        }}
      >
        {[
          { label: '+', title: 'Zoom in', onClick: () => zoomBy(1.4) },
          { label: '−', title: 'Zoom out', onClick: () => zoomBy(1 / 1.4) },
          { label: '⤢', title: 'Reset view', onClick: resetView },
        ].map((b) => (
          <button
            key={b.label}
            onClick={b.onClick}
            title={b.title}
            aria-label={b.title}
            style={{
              width: 30,
              height: 30,
              background: C.panel,
              border: `1px solid ${C.border}`,
              color: C.text,
              cursor: 'pointer',
              fontFamily: MONO,
              fontSize: 15,
              lineHeight: '15px',
              borderRadius: 3,
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
            }}
          >
            {b.label}
          </button>
        ))}
      </div>

      {/* ── Layer panel (bottom-left) ────────────────────────────────────────── */}
      <div
        style={{
          ...panel,
          position: 'absolute',
          bottom: 14,
          left: 14,
          width: 248,
          maxHeight: 'calc(100vh - 120px)',
          overflowY: 'auto',
          borderRadius: 3,
          padding: 10,
        }}
      >
        <div
          style={{
            color: C.dim,
            fontSize: 10,
            letterSpacing: '0.12em',
            marginBottom: 8,
            borderBottom: `1px solid ${C.border}`,
            paddingBottom: 6,
          }}
        >
          LAYERS
        </div>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          {legend.map((c) => {
            const on = !hidden.has(c.id)
            return (
              <button
                key={c.id}
                onClick={() => toggleContinent(c.id)}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 8,
                  padding: '4px 4px',
                  background: 'transparent',
                  border: 'none',
                  cursor: 'pointer',
                  textAlign: 'left',
                  opacity: on ? 1 : 0.4,
                  fontFamily: MONO,
                }}
              >
                <span
                  style={{
                    width: 11,
                    height: 11,
                    border: `1px solid ${C.border}`,
                    background: on ? c.color : 'transparent',
                    flexShrink: 0,
                    display: 'inline-flex',
                    alignItems: 'center',
                    justifyContent: 'center',
                    fontSize: 9,
                    color: C.bg,
                  }}
                >
                  {on ? '✓' : ''}
                </span>
                <span
                  style={{
                    width: 9,
                    height: 9,
                    borderRadius: '50%',
                    background: c.color,
                    flexShrink: 0,
                  }}
                />
                <span
                  style={{
                    flex: 1,
                    color: C.text,
                    fontSize: 10,
                    letterSpacing: '0.06em',
                    textTransform: 'uppercase',
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {c.label}
                </span>
                <span style={{ color: C.dim, fontSize: 10 }}>
                  {c.channelCount} · {c.msgs.toLocaleString()}
                </span>
              </button>
            )
          })}
        </div>
        <div
          style={{
            marginTop: 8,
            paddingTop: 6,
            borderTop: `1px solid ${C.border}`,
            color: C.dim,
            fontSize: 9,
            letterSpacing: '0.06em',
          }}
        >
          {totalChannels} CHANNELS · {totalMsgs.toLocaleString()} MSGS · LAST {windowLabel}
        </div>
      </div>

      {/* ── Recent activity ticker (bottom-right, hidden when a panel is open) ── */}
      {recentActivity.length > 0 && !selected && (
        <div
          style={{
            ...panel,
            position: 'absolute',
            bottom: 14,
            right: 14,
            width: 220,
            borderRadius: 3,
            padding: 10,
          }}
        >
          <div
            style={{
              color: C.dim,
              fontSize: 10,
              letterSpacing: '0.12em',
              marginBottom: 6,
              borderBottom: `1px solid ${C.border}`,
              paddingBottom: 6,
            }}
          >
            RECENT ACTIVITY
          </div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
            {recentActivity.map((a, i) => {
              const onMap = points.some((p) => p.channelId === a.channelId)
              return (
                <button
                  key={`${a.channelId}-${a.at}-${i}`}
                  type="button"
                  onClick={() => focusChannel(a.channelId)}
                  disabled={!onMap}
                  title={onMap ? `Open ${a.name}` : `${a.name} (not on map)`}
                  style={{
                    display: 'flex',
                    alignItems: 'center',
                    gap: 6,
                    fontSize: 10,
                    fontFamily: MONO,
                    background: 'none',
                    border: 'none',
                    padding: '1px 2px',
                    margin: 0,
                    textAlign: 'left',
                    cursor: onMap ? 'pointer' : 'default',
                    borderRadius: 2,
                  }}
                  onMouseEnter={(e) => {
                    if (onMap) e.currentTarget.style.background = C.border
                  }}
                  onMouseLeave={(e) => {
                    e.currentTarget.style.background = 'none'
                  }}
                >
                  <span style={{ color: C.green }}>▲</span>
                  <span
                    style={{
                      flex: 1,
                      color: C.text,
                      overflow: 'hidden',
                      textOverflow: 'ellipsis',
                      whiteSpace: 'nowrap',
                    }}
                  >
                    {a.name}
                  </span>
                  <span style={{ color: C.green }}>+{a.delta}</span>
                </button>
              )
            })}
          </div>
        </div>
      )}

      {/* ── Channel data panel (right side, opens on marker click) ───────────── */}
      {selected && (
        <div
          style={{
            ...panel,
            position: 'absolute',
            top: 48,
            right: 14,
            bottom: 14,
            width: 340,
            maxWidth: 'calc(100vw - 28px)',
            borderRadius: 3,
            padding: 12,
            display: 'flex',
            flexDirection: 'column',
            overflow: 'hidden',
          }}
        >
          <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 8 }}>
            <div style={{ minWidth: 0 }}>
              <div
                style={{
                  color: C.text,
                  fontSize: 13,
                  letterSpacing: '0.04em',
                  fontWeight: 600,
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  whiteSpace: 'nowrap',
                }}
              >
                {selected.name}
              </div>
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 4 }}>
                <span
                  style={{
                    width: 8,
                    height: 8,
                    borderRadius: '50%',
                    background: selected.color,
                    flexShrink: 0,
                  }}
                />
                <span
                  style={{
                    color: selected.color,
                    fontSize: 10,
                    letterSpacing: '0.06em',
                    textTransform: 'uppercase',
                  }}
                >
                  {selectedContinent?.label ?? selected.continentId}
                </span>
              </div>
              <div style={{ color: C.dim, fontSize: 10, marginTop: 4 }}>
                {selected.count.toLocaleString()} MSGS · LAST {windowLabel}
              </div>
            </div>
            <button
              onClick={() => setSelected(null)}
              style={{
                background: 'transparent',
                border: `1px solid ${C.border}`,
                color: C.dim,
                cursor: 'pointer',
                fontFamily: MONO,
                fontSize: 14,
                lineHeight: '14px',
                borderRadius: 2,
                padding: '2px 6px',
                flexShrink: 0,
              }}
              aria-label="Close"
            >
              ×
            </button>
          </div>

          <div
            style={{
              marginTop: 10,
              paddingTop: 8,
              borderTop: `1px solid ${C.border}`,
              color: C.dim,
              fontSize: 9,
              letterSpacing: '0.12em',
              marginBottom: 6,
            }}
          >
            TOPICS
            <span style={{ marginLeft: 6, fontFamily: MONO, fontSize: 8, color: C.dim, letterSpacing: '0.04em', textTransform: 'none' }}>
              times in {localTz}
            </span>
          </div>

          <div style={{ flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: 6 }}>
            {topicsLoading && <div style={{ color: C.dim, fontSize: 11 }}>Loading…</div>}
            {!topicsLoading && topics.length === 0 && (
              <div style={{ color: C.dim, fontSize: 11 }}>no topics in window</div>
            )}
            {!topicsLoading &&
              topics.map((t) => {
                const isNew = t.last_ms > lastSeen
                const isOpen = expanded.has(t.thread_ts)
                // "open in Slack": prefer the topic's url, else build a permalink
                // from the channel + thread_ts (digits, "." removed).
                let slackLink = t.url || ''
                if (!slackLink && t.thread_ts) {
                  const digits = t.thread_ts.replace('.', '')
                  if (digits) {
                    slackLink = `https://wego.slack.com/archives/${selected.channelId}/p${digits}`
                  }
                }
                const meta = [
                  t.participants && t.participants.length > 0 ? t.participants.join(', ') : '',
                  `${t.msg_count} msg${t.msg_count === 1 ? '' : 's'}`,
                  t.last_ms
                    ? new Date(t.last_ms).toLocaleString(undefined, {
                        month: 'short',
                        day: 'numeric',
                        hour: 'numeric',
                        minute: '2-digit',
                      })
                    : '',
                ]
                  .filter(Boolean)
                  .join(' · ')
                const msgs = threadMsgs[t.thread_ts]
                const loadingThread = threadLoading.has(t.thread_ts)
                return (
                  <div
                    key={t.thread_ts}
                    style={{
                      background: 'rgba(255,255,255,0.02)',
                      border: `1px solid ${C.border}`,
                      borderRadius: 3,
                      padding: 8,
                      display: 'flex',
                      flexDirection: 'column',
                      gap: 6,
                    }}
                  >
                    <div
                      style={{
                        display: 'flex',
                        alignItems: 'flex-start',
                        gap: 6,
                        cursor: t.is_thread ? 'pointer' : 'default',
                      }}
                      onClick={() => toggleTopic(t)}
                    >
                      <span
                        style={{
                          color: isNew ? C.green : C.dim,
                          fontSize: 11,
                          lineHeight: '15px',
                          flexShrink: 0,
                        }}
                      >
                        {isNew ? '◉' : '○'}
                      </span>
                      <div style={{ flex: 1, minWidth: 0 }}>
                        <div style={{ display: 'flex', alignItems: 'baseline', gap: 6 }}>
                          <span
                            style={{
                              flex: 1,
                              color: C.text,
                              fontSize: 11,
                              lineHeight: 1.35,
                              display: '-webkit-box',
                              WebkitLineClamp: 2,
                              WebkitBoxOrient: 'vertical',
                              overflow: 'hidden',
                            }}
                          >
                            {cfg ? applyGroupNames(t.summary, cfg) : t.summary}
                          </span>
                          {isNew && (
                            <span
                              style={{
                                flexShrink: 0,
                                color: C.green,
                                fontSize: 8,
                                letterSpacing: '0.08em',
                                border: `1px solid ${C.green}`,
                                borderRadius: 2,
                                padding: '0 3px',
                              }}
                            >
                              NEW
                            </span>
                          )}
                        </div>
                        <div
                          style={{
                            color: C.dim,
                            fontSize: 9,
                            marginTop: 3,
                            overflow: 'hidden',
                            textOverflow: 'ellipsis',
                            whiteSpace: 'nowrap',
                          }}
                        >
                          {meta}
                          {t.is_thread && <span style={{ marginLeft: 6 }}>{isOpen ? '▾' : '▸'}</span>}
                        </div>
                      </div>
                    </div>

                    {(slackLink || t.node_id) && (
                      <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
                        {slackLink && (
                          <a
                            href={slackLink}
                            target="_blank"
                            rel="noopener noreferrer"
                            onClick={(e) => e.stopPropagation()}
                            style={{
                              color: C.green,
                              fontSize: 9,
                              letterSpacing: '0.06em',
                              textDecoration: 'none',
                            }}
                          >
                            open in Slack ↗
                          </a>
                        )}
                        {t.node_id && (
                          <button
                            onClick={(e) => {
                              e.stopPropagation()
                              openGraph(t)
                            }}
                            style={{
                              background: 'transparent',
                              border: 'none',
                              padding: 0,
                              cursor: 'pointer',
                              fontFamily: MONO,
                              color: C.green,
                              fontSize: 9,
                              letterSpacing: '0.06em',
                            }}
                          >
                            open in Graph ↗
                          </button>
                        )}
                      </div>
                    )}

                    {t.is_thread && isOpen && (
                      <div
                        style={{
                          borderTop: `1px solid ${C.border}`,
                          paddingTop: 6,
                          display: 'flex',
                          flexDirection: 'column',
                          gap: 6,
                        }}
                      >
                        {/* Deep thread summary (overview + highlights), Slack-only. */}
                        {(t.overview || (t.highlights && t.highlights.length > 0)) && (
                          <div
                            style={{
                              display: 'flex',
                              flexDirection: 'column',
                              gap: 6,
                              padding: '8px 10px',
                              background: 'rgba(68,255,136,0.04)',
                              border: `1px solid ${C.border}`,
                              borderRadius: 4,
                            }}
                          >
                            <div
                              style={{
                                color: C.green,
                                fontSize: 8,
                                letterSpacing: '0.12em',
                                textTransform: 'uppercase',
                              }}
                            >
                              Thread summary
                            </div>
                            {t.overview && (
                              <div style={{ color: C.text, fontSize: 11, lineHeight: 1.5 }}>
                                {cfg ? applyGroupNames(t.overview, cfg) : t.overview}
                              </div>
                            )}
                            {t.highlights && t.highlights.length > 0 && (
                              <ul
                                style={{
                                  margin: 0,
                                  paddingLeft: 16,
                                  display: 'flex',
                                  flexDirection: 'column',
                                  gap: 3,
                                }}
                              >
                                {t.highlights.map((h, i) => (
                                  <li key={i} style={{ color: C.dim, fontSize: 10, lineHeight: 1.4 }}>
                                    {cfg ? applyGroupNames(h, cfg) : h}
                                  </li>
                                ))}
                              </ul>
                            )}
                          </div>
                        )}
                        {loadingThread && <div style={{ color: C.dim, fontSize: 10 }}>Loading…</div>}
                        {!loadingThread && msgs && msgs.length === 0 && (
                          <div style={{ color: C.dim, fontSize: 10 }}>no messages</div>
                        )}
                        {!loadingThread &&
                          msgs &&
                          [...msgs]
                            .sort((a, b) => a.ts_ms - b.ts_ms)
                            .map((m) => {
                              const raw = (m.title && m.title.trim()) || m.body || '(no content)'
                              const text = cfg ? applyGroupNames(raw, cfg) : raw
                              const ts = m.ts_ms
                                ? new Date(m.ts_ms).toLocaleString(undefined, {
                                    month: 'short',
                                    day: 'numeric',
                                    hour: 'numeric',
                                    minute: '2-digit',
                                  })
                                : ''
                              return (
                                <div
                                  key={m.id}
                                  style={{ display: 'flex', flexDirection: 'column', gap: 2 }}
                                >
                                  <div style={{ display: 'flex', alignItems: 'baseline', gap: 6 }}>
                                    <span style={{ color: C.dim, fontSize: 9, flexShrink: 0 }}>{ts}</span>
                                    {m.author && (
                                      <span style={{ color: C.dim, fontSize: 9, flexShrink: 0 }}>
                                        {m.author}
                                      </span>
                                    )}
                                  </div>
                                  <span style={{ color: C.text, fontSize: 11, lineHeight: 1.35 }}>
                                    {text}
                                  </span>
                                  {m.refs && m.refs.length > 0 && (
                                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginTop: 2 }}>
                                      {m.refs.map((ref, i) => {
                                        const label = ref.key ? `↗ ${ref.key}` : `${ref.type}:${ref.key}`
                                        const chipStyle: React.CSSProperties = {
                                          fontFamily: MONO,
                                          fontSize: 9,
                                          letterSpacing: '0.04em',
                                          padding: '1px 5px',
                                          border: `1px solid ${C.border}`,
                                          borderRadius: 2,
                                          color: C.dim,
                                          textDecoration: 'none',
                                          whiteSpace: 'nowrap',
                                        }
                                        return ref.url ? (
                                          <a
                                            key={`${ref.type}-${ref.key}-${i}`}
                                            href={ref.url}
                                            target="_blank"
                                            rel="noopener noreferrer"
                                            style={chipStyle}
                                          >
                                            {label}
                                          </a>
                                        ) : (
                                          <span key={`${ref.type}-${ref.key}-${i}`} style={chipStyle}>
                                            {label}
                                          </span>
                                        )
                                      })}
                                    </div>
                                  )}
                                </div>
                              )
                            })}
                      </div>
                    )}
                  </div>
                )
              })}
          </div>
        </div>
      )}

      {/* ── Subscriptions overlay: enzobot hot-topic DM alerts ──────────────── */}
      {subsOpen && (
        <div
          onClick={() => setSubsOpen(false)}
          style={{
            position: 'absolute',
            inset: 0,
            background: 'rgba(0,0,0,0.55)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            zIndex: 20,
          }}
        >
          <div
            onClick={(e) => e.stopPropagation()}
            style={{
              ...panel,
              width: 'min(520px, calc(100vw - 32px))',
              maxHeight: 'calc(100vh - 64px)',
              borderRadius: 4,
              padding: 16,
              display: 'flex',
              flexDirection: 'column',
              overflow: 'hidden',
            }}
          >
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
              <div style={{ color: C.text, fontSize: 13, fontWeight: 600, letterSpacing: '0.04em' }}>
                🔔 HOT-TOPIC ALERTS
              </div>
              <button
                onClick={() => setSubsOpen(false)}
                style={{
                  background: 'transparent',
                  border: `1px solid ${C.border}`,
                  color: C.dim,
                  cursor: 'pointer',
                  fontFamily: MONO,
                  fontSize: 14,
                  lineHeight: '14px',
                  borderRadius: 2,
                  padding: '2px 6px',
                }}
                aria-label="Close"
              >
                ×
              </button>
            </div>
            <div style={{ color: C.dim, fontSize: 10, lineHeight: 1.5, marginTop: 8 }}>
              enzobot DMs you when a Slack thread matching a topic gets hot — a senior
              person raises it, or many people start discussing it.
            </div>

            {/* Add form */}
            <form
              onSubmit={(e) => {
                e.preventDefault()
                addSub()
              }}
              style={{ display: 'flex', gap: 6, marginTop: 12, flexWrap: 'wrap' }}
            >
              <input
                value={subTopic}
                onChange={(e) => setSubTopic(e.target.value)}
                placeholder="topic, e.g. payments"
                style={{
                  flex: 2,
                  minWidth: 140,
                  background: C.panel,
                  border: `1px solid ${C.border}`,
                  color: C.text,
                  fontFamily: MONO,
                  fontSize: 11,
                  padding: '6px 8px',
                  borderRadius: 2,
                  outline: 'none',
                }}
              />
              <input
                value={subChannel}
                onChange={(e) => setSubChannel(e.target.value)}
                placeholder="channel id (optional)"
                style={{
                  flex: 1,
                  minWidth: 120,
                  background: C.panel,
                  border: `1px solid ${C.border}`,
                  color: C.text,
                  fontFamily: MONO,
                  fontSize: 11,
                  padding: '6px 8px',
                  borderRadius: 2,
                  outline: 'none',
                }}
              />
              {/* Dynamic knowledge sources: type dropdown + URL + add/remove */}
              <div style={{ flex: '1 1 100%', display: 'flex', flexDirection: 'column', gap: 6 }}>
                <SourceRows
                  sources={subSources}
                  onUpdate={updateSource}
                  onRemove={removeSource}
                  onAdd={addSourceRow}
                />
              </div>
              <button type="submit" disabled={subBusy || !subTopic.trim()} style={segBtn(true)}>
                {subBusy ? '…' : 'SUBSCRIBE'}
              </button>
            </form>
            <div style={{ color: C.dim, fontSize: 9, marginTop: 6, lineHeight: 1.5 }}>
              Add knowledge sources (Confluence, GitHub, Slack, Google Docs, Wego Hub, …) that define
              the topic. After subscribing (or via ↻ refresh), enzobot reads + analyzes them and shows
              a scope summary below — so we agree on what the topic means before alerting.
            </div>
            {subError && (
              <div style={{ color: C.red, fontSize: 10, marginTop: 6 }}>{subError}</div>
            )}

            {/* List */}
            <div
              style={{
                marginTop: 14,
                paddingTop: 10,
                borderTop: `1px solid ${C.border}`,
                flex: 1,
                overflowY: 'auto',
                display: 'flex',
                flexDirection: 'column',
                gap: 6,
              }}
            >
              {subs.length === 0 && (
                <div style={{ color: C.dim, fontSize: 11 }}>no subscriptions yet</div>
              )}
              {subs.map((s) => {
                const refreshing = s.scope_status === 'refreshing'
                return (
                  <div
                    key={s.id}
                    style={{
                      display: 'flex',
                      flexDirection: 'column',
                      gap: 6,
                      background: 'rgba(255,255,255,0.02)',
                      border: `1px solid ${C.border}`,
                      borderRadius: 3,
                      padding: 8,
                    }}
                  >
                    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                      <div style={{ flex: 1, minWidth: 0 }}>
                        <div style={{ color: C.text, fontSize: 12 }}>
                          {s.topic}
                          {!s.active && <span style={{ color: C.dim, fontSize: 9 }}> · paused</span>}
                        </div>
                        <div style={{ color: C.dim, fontSize: 9, marginTop: 2 }}>
                          {s.channel_filter.length > 0 ? `#${s.channel_filter.join(', #')}` : 'all channels'}
                          {' · '}≥{s.min_participants} people discussing
                          {s.sources && s.sources.length > 0 && ` · ${s.sources.length} source${s.sources.length === 1 ? '' : 's'}`}
                        </div>
                      </div>
                      {(s.sources?.length ?? 0) > 0 && (
                        <button
                          onClick={() => refreshSubScope(s.id)}
                          disabled={refreshing}
                          title="Re-read + analyze the sources"
                          style={{
                            background: 'transparent',
                            border: `1px solid ${refreshing ? C.green : C.border}`,
                            color: refreshing ? C.green : C.dim,
                            cursor: refreshing ? 'default' : 'pointer',
                            fontFamily: MONO,
                            fontSize: 10,
                            borderRadius: 2,
                            padding: '2px 8px',
                          }}
                        >
                          {refreshing ? 'analyzing…' : '↻ refresh'}
                        </button>
                      )}
                      <button
                        onClick={() => (editingSub === s.id ? setEditingSub(null) : startEdit(s))}
                        title="Add or edit knowledge sources"
                        style={{
                          background: 'transparent',
                          border: `1px solid ${editingSub === s.id ? C.green : C.border}`,
                          color: editingSub === s.id ? C.green : C.dim,
                          cursor: 'pointer',
                          fontFamily: MONO,
                          fontSize: 10,
                          borderRadius: 2,
                          padding: '2px 8px',
                        }}
                      >
                        {editingSub === s.id ? 'close' : '✎ sources'}
                      </button>
                      <button
                        onClick={() => removeSub(s.id)}
                        style={{
                          background: 'transparent',
                          border: `1px solid ${C.border}`,
                          color: C.dim,
                          cursor: 'pointer',
                          fontFamily: MONO,
                          fontSize: 10,
                          borderRadius: 2,
                          padding: '2px 8px',
                        }}
                      >
                        delete
                      </button>
                    </div>
                    {(s.sources?.length ?? 0) > 0 && (
                      <div style={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
                        {s.sources!.map((src, i) => {
                          const label = SOURCE_TYPES.find((o) => o.value === src.type)?.label ?? src.type
                          return (
                            <a
                              key={i}
                              href={src.url}
                              target="_blank"
                              rel="noopener noreferrer"
                              title={src.url}
                              style={{
                                display: 'flex',
                                gap: 6,
                                alignItems: 'baseline',
                                textDecoration: 'none',
                                fontSize: 9,
                              }}
                            >
                              <span style={{ color: C.green, flexShrink: 0, minWidth: 92 }}>↗ {label}</span>
                              <span
                                style={{
                                  color: C.dim,
                                  overflow: 'hidden',
                                  textOverflow: 'ellipsis',
                                  whiteSpace: 'nowrap',
                                }}
                              >
                                {src.url}
                              </span>
                            </a>
                          )
                        })}
                      </div>
                    )}
                    {s.scope_summary && (
                      <div
                        style={{
                          color: C.dim,
                          fontSize: 10,
                          lineHeight: 1.5,
                          padding: '6px 8px',
                          background: 'rgba(68,255,136,0.04)',
                          border: `1px solid ${C.border}`,
                          borderRadius: 3,
                        }}
                      >
                        <span style={{ color: C.green, fontSize: 8, letterSpacing: '0.1em', textTransform: 'uppercase' }}>
                          Scope
                        </span>
                        <div style={{ marginTop: 3 }}>{s.scope_summary}</div>
                      </div>
                    )}
                    {editingSub === s.id && (
                      <div
                        style={{
                          display: 'flex',
                          flexDirection: 'column',
                          gap: 6,
                          paddingTop: 8,
                          borderTop: `1px solid ${C.border}`,
                        }}
                      >
                        <SourceRows
                          sources={editSources}
                          onUpdate={editUpdate}
                          onRemove={editRemove}
                          onAdd={editAddRow}
                        />
                        <button
                          type="button"
                          onClick={() => saveEdit(s.id)}
                          disabled={subBusy}
                          style={segBtn(true)}
                        >
                          {subBusy ? '…' : 'SAVE + ANALYZE'}
                        </button>
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
          </div>
        </div>
      )}

      {/* ── "Open in Graph" overlay: this thread's related cross-source resources ─ */}
      {graphTopic && (
        <div
          onClick={() => setGraphTopic(null)}
          style={{
            position: 'absolute',
            inset: 0,
            background: 'rgba(0,0,0,0.55)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            zIndex: 20,
          }}
        >
          <div
            onClick={(e) => e.stopPropagation()}
            style={{
              ...panel,
              width: 'min(880px, calc(100vw - 32px))',
              maxHeight: 'calc(100vh - 48px)',
              borderRadius: 4,
              padding: 16,
              display: 'flex',
              flexDirection: 'column',
              overflow: 'hidden',
            }}
          >
            <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 8 }}>
              <div style={{ minWidth: 0 }}>
                {graphStack.length > 1 && (
                  <button
                    onClick={graphBack}
                    style={{
                      background: 'transparent',
                      border: 'none',
                      color: C.dim,
                      cursor: 'pointer',
                      fontFamily: MONO,
                      fontSize: 10,
                      padding: 0,
                      marginBottom: 6,
                      letterSpacing: '0.08em',
                    }}
                  >
                    ‹ back · {graphStack.length} levels
                  </button>
                )}
                <div
                  style={{
                    color: C.text,
                    fontSize: 13,
                    lineHeight: 1.4,
                    fontWeight: 600,
                    display: '-webkit-box',
                    WebkitLineClamp: 2,
                    WebkitBoxOrient: 'vertical',
                    overflow: 'hidden',
                  }}
                >
                  {graphStack[graphStack.length - 1]?.label ??
                    (cfg ? applyGroupNames(graphTopic.summary, cfg) : graphTopic.summary)}
                </div>
                <div style={{ display: 'flex', gap: 4, marginTop: 8 }}>
                  {(['summary', 'diagram'] as const).map((v) => (
                    <button
                      key={v}
                      onClick={() => setGraphView(v)}
                      style={{
                        background: graphView === v ? 'rgba(68,255,136,0.12)' : 'transparent',
                        border: `1px solid ${graphView === v ? C.green : C.border}`,
                        color: graphView === v ? C.green : C.dim,
                        cursor: 'pointer',
                        fontFamily: MONO,
                        fontSize: 9,
                        letterSpacing: '0.1em',
                        textTransform: 'uppercase',
                        borderRadius: 3,
                        padding: '3px 8px',
                      }}
                    >
                      {v === 'summary' ? 'Summary' : 'Graph'}
                    </button>
                  ))}
                </div>
              </div>
              <button
                onClick={() => setGraphTopic(null)}
                style={{
                  background: 'transparent',
                  border: `1px solid ${C.border}`,
                  color: C.dim,
                  cursor: 'pointer',
                  fontFamily: MONO,
                  fontSize: 14,
                  lineHeight: '14px',
                  borderRadius: 2,
                  padding: '2px 6px',
                  flexShrink: 0,
                }}
                aria-label="Close"
              >
                ×
              </button>
            </div>

            <div
              style={{
                marginTop: 12,
                paddingTop: 12,
                borderTop: `1px solid ${C.border}`,
                flex: 1,
                overflowY: 'auto',
                display: 'flex',
                flexDirection: 'column',
                gap: 14,
              }}
            >
              {graphView === 'diagram' &&
                (() => {
                  const rootId = graphStack[graphStack.length - 1]?.id ?? graphTopic.node_id
                  const s = summaryCache[rootId]
                  if (!s || s === 'loading')
                    return <div style={{ color: C.dim, fontSize: 11 }}>Loading graph…</div>
                  let nodes = s.nodes
                  let edges = s.edges
                  if (!nodes || nodes.length <= 1 || !edges || edges.length === 0) {
                    // No explicit edge topology (e.g. only SIMILAR semantic
                    // neighbors). Fall back to a star of the same neighbors the
                    // SUMMARY tab lists, so the two views never disagree.
                    const nbrs = neighborCache[rootId]
                    if (!nbrs && graphLoading)
                      return <div style={{ color: C.dim, fontSize: 11 }}>Loading graph…</div>
                    const flat = groupNeighbors(nbrs ?? []).flatMap((g) => g.items)
                    if (flat.length === 0)
                      return <div style={{ color: C.dim, fontSize: 11 }}>no graph for this node</div>
                    const rootLabel =
                      graphStack[graphStack.length - 1]?.label || graphTopic.summary || rootId
                    nodes = [
                      { id: rootId, type: 'slack', title: rootLabel, url: '', root: true },
                      ...flat.map((n) => ({
                        id: n.node.node_id,
                        type: n.node.type,
                        title: n.node.title || n.node.node_id.replace(/^[a-z_]+:/, ''),
                        url: n.node.url,
                      })),
                    ]
                    edges = flat.map((n) => ({ from: rootId, to: n.node.node_id, kind: n.edge.kind }))
                  }
                  return (
                    <ClusterGraph
                      nodes={nodes}
                      edges={edges}
                      width={Math.min(512, window.innerWidth - 64)}
                      height={440}
                      onDrill={(id, label) => {
                        setGraphView('summary')
                        drillInto(id, label)
                      }}
                    />
                  )
                })()}
              {graphView === 'summary' &&
                (() => {
                const rootId = graphStack[graphStack.length - 1]?.id ?? graphTopic.node_id
                const s = summaryCache[rootId]
                if (!s) return null
                if (s === 'loading') {
                  return <div style={{ color: C.dim, fontSize: 11 }}>Summarizing…</div>
                }
                if (!s.overview && s.highlights.length === 0) return null
                // Chips tally the same neighbor groups shown in the sections below, so
                // the counts always match (was a separate bounded-BFS count that disagreed).
                const chipGroups = groupNeighbors(neighborCache[rootId] ?? [])
                return (
                  <div
                    style={{
                      display: 'flex',
                      flexDirection: 'column',
                      gap: 8,
                      padding: '10px 12px',
                      background: 'rgba(68,255,136,0.04)',
                      border: `1px solid ${C.border}`,
                      borderRadius: 4,
                    }}
                  >
                    <div
                      style={{
                        color: C.green,
                        fontSize: 9,
                        letterSpacing: '0.12em',
                        textTransform: 'uppercase',
                      }}
                    >
                      Summary
                    </div>
                    {s.overview && (
                      <div style={{ color: C.text, fontSize: 12, lineHeight: 1.5 }}>{s.overview}</div>
                    )}
                    {s.highlights.length > 0 && (
                      <ul style={{ margin: 0, paddingLeft: 16, display: 'flex', flexDirection: 'column', gap: 4 }}>
                        {s.highlights.map((h, i) => (
                          <li key={i} style={{ color: C.dim, fontSize: 11, lineHeight: 1.45 }}>
                            {cfg ? applyGroupNames(h, cfg) : h}
                          </li>
                        ))}
                      </ul>
                    )}
                    {chipGroups.length > 0 && (
                      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginTop: 2 }}>
                        {chipGroups.map((g) => (
                          <span
                            key={g.label}
                            style={{
                              color: C.dim,
                              fontSize: 10,
                              border: `1px solid ${C.border}`,
                              borderRadius: 3,
                              padding: '2px 6px',
                            }}
                          >
                            {g.label} · {g.items.length}
                          </span>
                        ))}
                      </div>
                    )}
                  </div>
                )
              })()}
              {graphView === 'summary' &&
                graphLoading &&
                !neighborCache[graphStack[graphStack.length - 1]?.id ?? graphTopic.node_id] && (
                  <div style={{ color: C.dim, fontSize: 11 }}>Loading…</div>
                )}
              {graphView === 'summary' &&
                (() => {
                const rootId = graphStack[graphStack.length - 1]?.id ?? graphTopic.node_id
                const neighbors = neighborCache[rootId]
                if (!neighbors) return null
                if (neighbors.length === 0) {
                  return <div style={{ color: C.dim, fontSize: 11 }}>no linked resources yet</div>
                }
                const groups = groupNeighbors(neighbors)
                // Row numbers follow display order (group by group) and repeat on
                // the timeline dots, so a dot can be matched to its row.
                const rowNo = new Map<string, number>()
                let nextNo = 1
                for (const g of groups) for (const n of g.items) rowNo.set(n.node.node_id, nextNo++)
                return (
                  <>
                    <NeighborTimeline neighbors={neighbors} numbers={rowNo} />
                    {groups.map((g) => (
                  <div key={g.label} style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                    <div
                      style={{
                        display: 'flex',
                        alignItems: 'baseline',
                        gap: 6,
                        color: C.dim,
                        fontSize: 10,
                        letterSpacing: '0.1em',
                        textTransform: 'uppercase',
                      }}
                    >
                      <span>{g.label}</span>
                      <span style={{ color: C.dim, opacity: 0.7 }}>· {g.items.length}</span>
                    </div>
                    {g.items.map((n, i) => {
                      // Untitled node: show the bare key ("122349"), not the raw
                      // "jira_attachment:122349" — the group header names the type.
                      const label = n.node.title || n.node.node_id.replace(/^[a-z_]+:/, '')
                      const hint = neighborHint(n)
                      const itemStyle: React.CSSProperties = {
                        display: 'flex',
                        alignItems: 'baseline',
                        gap: 8,
                        background: 'rgba(255,255,255,0.02)',
                        border: `1px solid ${C.border}`,
                        borderRadius: 3,
                        padding: '6px 8px',
                        textDecoration: 'none',
                        color: C.text,
                      }
                      const inner = (
                        <>
                          <span
                            style={{
                              flexShrink: 0,
                              minWidth: 16,
                              textAlign: 'right',
                              color: C.dim,
                              fontSize: 9,
                            }}
                          >
                            {rowNo.get(n.node.node_id)}
                          </span>
                          <span
                            style={{
                              flex: 1,
                              minWidth: 0,
                              color: C.text,
                              fontSize: 11,
                              lineHeight: 1.35,
                              overflow: 'hidden',
                              textOverflow: 'ellipsis',
                              whiteSpace: 'nowrap',
                            }}
                          >
                            {label}
                          </span>
                          {(n.node.first_ts_ms ?? 0) > 0 && (
                            <span
                              title={`created ${fmtDateHM(n.node.first_ts_ms!)}${
                                (n.node.last_ts_ms ?? 0) > n.node.first_ts_ms!
                                  ? ` · last message ${fmtDateHM(n.node.last_ts_ms!)}`
                                  : ''
                              }`}
                              style={{
                                flexShrink: 0,
                                color: C.dim,
                                fontSize: 9,
                                opacity: 0.85,
                                whiteSpace: 'nowrap',
                              }}
                            >
                              {fmtDateHM(n.node.first_ts_ms!)}
                              {(n.node.last_ts_ms ?? 0) - n.node.first_ts_ms! > 60_000
                                ? ` → ${fmtDateHM(n.node.last_ts_ms!)}`
                                : ''}
                            </span>
                          )}
                          {n.node.channel && (
                            <span
                              style={{
                                flexShrink: 0,
                                color: C.dim,
                                fontSize: 9,
                                opacity: 0.85,
                              }}
                            >
                              #{n.node.channel}
                            </span>
                          )}
                          {hint && (
                            <span
                              style={{
                                flexShrink: 0,
                                color: C.dim,
                                fontSize: 9,
                                opacity: 0.85,
                              }}
                            >
                              {hint}
                            </span>
                          )}
                          <span
                            title={edgeKindTooltip(n.edge)}
                            style={{
                              flexShrink: 0,
                              color: C.dim,
                              fontSize: 8,
                              letterSpacing: '0.06em',
                              textTransform: 'uppercase',
                              cursor: 'help',
                            }}
                          >
                            {n.edge.kind}
                            {n.edge.kind.toUpperCase() === 'SIMILAR' && n.edge.score
                              ? ` ${Math.round(n.edge.score * 100)}%`
                              : ''}
                          </span>
                        </>
                      )
                      const drillBtn = (
                        <button
                          onClick={(e) => {
                            e.preventDefault()
                            e.stopPropagation()
                            drillInto(n.node.node_id, label)
                          }}
                          title="Expand this node's links"
                          style={{
                            flexShrink: 0,
                            background: 'transparent',
                            border: `1px solid ${C.border}`,
                            borderRadius: 3,
                            color: C.dim,
                            cursor: 'pointer',
                            fontFamily: MONO,
                            fontSize: 12,
                            lineHeight: '12px',
                            padding: '0 8px',
                          }}
                        >
                          ⤵
                        </button>
                      )
                      return (
                        <div key={`${n.node.node_id}-${i}`} style={{ display: 'flex', gap: 6 }}>
                          {n.node.url ? (
                            <a
                              href={n.node.url}
                              target="_blank"
                              rel="noopener noreferrer"
                              style={{ ...itemStyle, flex: 1, minWidth: 0 }}
                            >
                              {inner}
                            </a>
                          ) : (
                            <span style={{ ...itemStyle, flex: 1, minWidth: 0 }}>{inner}</span>
                          )}
                          {drillBtn}
                        </div>
                      )
                    })}
                  </div>
                ))}
                  </>
                )
              })()}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
