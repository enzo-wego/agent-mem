import { useEffect, useMemo, useRef, useState } from 'react'
import {
  fetchChannels,
  fetchContinents,
  fetchChannelMessages,
  type ChannelCount,
  type ContinentCfg,
  type ChannelMessage,
} from '../api'
import { assignCountries, continentOf, nameOf } from '../continents'

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

const REFRESH_OPTIONS = [5, 10, 30] as const
const WINDOW_OPTIONS = [
  { days: 90, label: '3 MONTHS' },
  { days: 365, label: 'THIS YEAR' },
  { days: 0, label: 'ALL' },
] as const
const PULSE_MS = 2000

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
  const [messages, setMessages] = useState<ChannelMessage[]>([])
  const [msgsLoading, setMsgsLoading] = useState(false)

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

  // ── Click panel: (re)load messages for the selected channel + window ─────────
  useEffect(() => {
    if (!selected) return
    let cancelled = false
    setMsgsLoading(true)
    setMessages([])
    fetchChannelMessages(selected.channelId, windowDays, 20)
      .then((m) => {
        if (!cancelled) setMessages(m || [])
      })
      .catch(() => {
        if (!cancelled) setMessages([])
      })
      .finally(() => {
        if (!cancelled) setMsgsLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [selected, windowDays])

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
        viewBox="0 0 360 180"
        preserveAspectRatio="xMidYMid meet"
        style={{ position: 'absolute', inset: 0, width: '100%', height: '100%', background: C.bg }}
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

        {/* Countries */}
        <g>
          {countryPaths.map((d, i) => (
            <path key={i} d={d} fill={C.land} stroke={C.landStroke} strokeWidth={0.15} />
          ))}
        </g>

        {/* Channel markers (visible continents only) */}
        <g>
          {visiblePts.map((p) => {
            const cx = projX(p.lng)
            const cy = projY(p.lat)
            const r = radiusFor(p.count)
            const isHover = hovered === p.channelId
            const isSel = selected?.channelId === p.channelId
            const pulsing = p.pulseUntil > now
            return (
              <g
                key={p.channelId}
                style={{ cursor: 'pointer' }}
                onMouseEnter={() => setHovered(p.channelId)}
                onMouseLeave={() => setHovered((h) => (h === p.channelId ? null : h))}
                onClick={() => setSelected(p)}
              >
                {/* soft halo */}
                <circle cx={cx} cy={cy} r={r * 2.2} fill={p.color} opacity={isHover || isSel ? 0.22 : 0.12} />
                {/* expanding ring when count just grew */}
                {pulsing && (
                  <circle cx={cx} cy={cy} fill="none" stroke={p.color} strokeWidth={0.4} opacity={0.8}>
                    <animate attributeName="r" from={r} to={r + 9} dur="2s" repeatCount="indefinite" />
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
                  strokeWidth={isSel ? 0.4 : 0}
                  filter="url(#marker-glow)"
                />
                {/* channel name label (no country name) */}
                <text
                  x={cx + r + 1}
                  y={cy + 0.7}
                  fontSize={2}
                  fontFamily={MONO}
                  fill="#cbd5e1"
                  style={{ paintOrder: 'stroke', pointerEvents: 'none' }}
                  stroke="#0a0a0a"
                  strokeWidth={0.4}
                >
                  {p.name}
                </text>
              </g>
            )
          })}
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
            LIVE — GLOBAL CHANNEL ACTIVITY
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
                <span style={{ color: C.dim, fontSize: 10 }}>· {selected.country}</span>
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
            RECENT MESSAGES
          </div>

          <div style={{ flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: 6 }}>
            {msgsLoading && <div style={{ color: C.dim, fontSize: 11 }}>Loading…</div>}
            {!msgsLoading && messages.length === 0 && (
              <div style={{ color: C.dim, fontSize: 11 }}>no messages in window</div>
            )}
            {!msgsLoading &&
              messages.map((m) => {
                const raw = (m.title && m.title.trim()) || (m.body || '').split('\n')[0] || '(no content)'
                const text = raw.length > 120 ? `${raw.slice(0, 120)}…` : raw
                const ts = m.ts ? new Date(m.ts).toLocaleString(undefined, {
                  month: 'short',
                  day: 'numeric',
                  hour: '2-digit',
                  minute: '2-digit',
                }) : ''
                const Row = (
                  <>
                    <span style={{ color: C.dim, fontSize: 9, flexShrink: 0 }}>{ts}</span>
                    <span style={{ color: C.text, fontSize: 11, lineHeight: 1.35 }}>{text}</span>
                  </>
                )
                return m.url ? (
                  <a
                    key={m.id}
                    href={m.url}
                    target="_blank"
                    rel="noreferrer"
                    style={{
                      display: 'flex',
                      flexDirection: 'column',
                      gap: 2,
                      textDecoration: 'none',
                      paddingBottom: 6,
                      borderBottom: `1px solid ${C.border}`,
                    }}
                  >
                    {Row}
                  </a>
                ) : (
                  <div
                    key={m.id}
                    style={{
                      display: 'flex',
                      flexDirection: 'column',
                      gap: 2,
                      paddingBottom: 6,
                      borderBottom: `1px solid ${C.border}`,
                    }}
                  >
                    {Row}
                  </div>
                )
              })}
          </div>
        </div>
      )}
    </div>
  )
}
