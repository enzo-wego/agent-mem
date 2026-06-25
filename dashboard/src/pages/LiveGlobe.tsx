import { useEffect, useMemo, useRef, useState } from 'react'
import {
  fetchChannels,
  fetchContinents,
  fetchChannelMessages,
  fetchChannelTopics,
  fetchNeighbors,
  fetchClusterSummary,
  type ChannelCount,
  type ContinentCfg,
  type ChannelMessage,
  type ChannelTopic,
  type GraphNeighbor,
  type ClusterSummary,
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
  person: { label: 'People', order: 5 },
  gws_doc: { label: 'Google Docs', order: 2 },
  gws: { label: 'Google Docs', order: 2 },
  feature: { label: 'Features', order: 6 },
}

interface NeighborGroup {
  label: string
  order: number
  items: GraphNeighbor[]
}

// Group neighbors by friendly type label, preserving server order within a group
// and sorting groups so Jira/PRs/Confluence/Slack come first.
function groupNeighbors(neighbors: GraphNeighbor[]): NeighborGroup[] {
  const byLabel = new Map<string, NeighborGroup>()
  for (const n of neighbors) {
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
  return [...byLabel.values()].sort((a, b) => a.order - b.order || a.label.localeCompare(b.label))
}

const REFRESH_OPTIONS = [5, 10, 30] as const
const WINDOW_OPTIONS = [
  { days: 90, label: '3 MONTHS' },
  { days: 365, label: 'THIS YEAR' },
  { days: 0, label: 'ALL' },
] as const
const PULSE_MS = 2000

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
    const cid = continentOf(ch.channel_id, cfg)
    pts.push({
      channelId: ch.channel_id,
      name: nameOf(ch.channel_id, cfg),
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
        const increases: ActivityEntry[] = []
        const assigned = assignCountries(channels || [], cfgNow)
        for (const ch of channels || []) {
          const before = prev.get(ch.channel_id)
          if (before !== undefined && ch.count > before) {
            pulse.set(ch.channel_id, now + PULSE_MS)
            increases.push({
              country: assigned[ch.channel_id]?.name ?? '',
              name: nameOf(ch.channel_id, cfgNow),
              delta: ch.count - before,
              at: now,
            })
          }
        }
        // Record current counts for the next diff.
        const nextCounts = new Map<string, number>()
        for (const ch of channels || []) nextCounts.set(ch.channel_id, ch.count)
        prevCountsRef.current = nextCounts

        setPoints(buildPoints(channels || [], cfgNow, pulse))
        setLastUpdate(now)
        setError('')
        if (increases.length > 0) {
          increases.sort((a, b) => b.delta - a.delta)
          setActivity((cur) => [...increases.slice(0, 5), ...cur].slice(0, 6))
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
          [nodeId]: { overview: '', highlights: [], resources: [], nodes: [], edges: [], node_count: 0 },
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

  const visiblePts = points.filter((p) => !hidden.has(p.continentId))
  const maxCount = Math.max(1, ...visiblePts.map((p) => p.count))
  const totalChannels = visiblePts.length
  const totalMsgs = visiblePts.reduce((s, p) => s + p.count, 0)
  const secsAgo = lastUpdate ? Math.max(0, Math.round((Date.now() - lastUpdate) / 1000)) : null
  const windowLabel = WINDOW_OPTIONS.find((w) => w.days === windowDays)?.label ?? 'ALL'
  const now = Date.now()
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

  const k = view.k

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
            const fontPrimary = 2 / k
            const fontSecondary = fontPrimary * 0.7
            const labelStroke = 0.4 / k
            const hasName = p.name !== p.channelId
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
                {/* label: name on line 1, raw id on line 2 (id-only if unnamed) */}
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
                {hasName && (
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
      {activity.length > 0 && !selected && (
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
            {activity.map((a, i) => (
              <div
                key={`${a.name}-${a.at}-${i}`}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 6,
                  fontSize: 10,
                  fontFamily: MONO,
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
              </div>
            ))}
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
              width: 'min(560px, calc(100vw - 32px))',
              maxHeight: 'calc(100vh - 64px)',
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
                  if (!s.nodes || s.nodes.length === 0)
                    return <div style={{ color: C.dim, fontSize: 11 }}>no graph for this node</div>
                  return (
                    <ClusterGraph
                      nodes={s.nodes}
                      edges={s.edges}
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
                if (!s.overview && s.resources.length === 0) return null
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
                    {s.resources.length > 0 && (
                      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginTop: 2 }}>
                        {s.resources.map((r) => (
                          <span
                            key={r.source}
                            style={{
                              color: C.dim,
                              fontSize: 10,
                              border: `1px solid ${C.border}`,
                              borderRadius: 3,
                              padding: '2px 6px',
                            }}
                          >
                            {r.source} · {r.count}
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
                return groupNeighbors(neighbors).map((g) => (
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
                      const label = n.node.title || n.node.node_id
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
                          <span
                            style={{
                              flexShrink: 0,
                              color: C.dim,
                              fontSize: 8,
                              letterSpacing: '0.06em',
                              textTransform: 'uppercase',
                            }}
                          >
                            {n.edge.kind}
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
                ))
              })()}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
