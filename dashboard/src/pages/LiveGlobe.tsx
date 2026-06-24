import { useEffect, useMemo, useRef, useState } from 'react'
import Globe, { type GlobeInstance } from 'globe.gl'
import { fetchChannels, fetchContinents, type ChannelCount, type ContinentCfg } from '../api'
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
} as const

const MONO = 'ui-monospace, "SF Mono", Menlo, monospace'

const REFRESH_OPTIONS = [5, 10, 30] as const
const WINDOW_OPTIONS = [
  { days: 90, label: '3 MONTHS' },
  { days: 365, label: 'THIS YEAR' },
  { days: 0, label: 'ALL' },
] as const
const PULSE_MS = 2000

// Marker pixel radii (sqrt-scaled by count, clamped so tiny channels stay
// visible and the biggest doesn't dominate the globe).
const MIN_R = 5
const MAX_R = 26

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

function lighten(hex: string, amount: number): string {
  // Lighten a #rrggbb color toward white by `amount` (0..1). Falls back to input.
  const m = /^#?([0-9a-f]{6})$/i.exec(hex)
  if (!m) return hex
  const n = parseInt(m[1], 16)
  const r = (n >> 16) & 0xff
  const g = (n >> 8) & 0xff
  const b = n & 0xff
  const lr = Math.round(r + (255 - r) * amount)
  const lg = Math.round(g + (255 - g) * amount)
  const lb = Math.round(b + (255 - b) * amount)
  return `#${((1 << 24) + (lr << 16) + (lg << 8) + lb).toString(16).slice(1)}`
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

export function LiveGlobePage() {
  const containerRef = useRef<HTMLDivElement>(null)
  const globeRef = useRef<GlobeInstance | null>(null)

  const [cfg, setCfg] = useState<ContinentCfg | null>(null)
  const [points, setPoints] = useState<LivePoint[]>([])
  const [hidden, setHidden] = useState<Set<string>>(new Set())
  const [interval, setIntervalSec] = useState<number>(10)
  const [windowDays, setWindowDays] = useState<number>(90)
  const [lastUpdate, setLastUpdate] = useState<number>(0)
  const [tick, setTick] = useState(0)
  const [error, setError] = useState('')
  const [activity, setActivity] = useState<ActivityEntry[]>([])

  // Refs mirror state for use inside the polling closure without re-subscribing.
  const prevCountsRef = useRef<Map<string, number>>(new Map())
  const cfgRef = useRef<ContinentCfg | null>(null)
  const inFlightRef = useRef(false)
  const windowDaysRef = useRef(90)
  windowDaysRef.current = windowDays

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

  // ── Create the globe once ───────────────────────────────────────────────────
  useEffect(() => {
    if (!containerRef.current || globeRef.current) return
    const g = new Globe(containerRef.current)
      .globeImageUrl('/textures/earth-topo-bathy.jpg')
      .backgroundImageUrl('/textures/night-sky.png')
      .backgroundColor(C.bg)
      .showAtmosphere(true)
      .atmosphereColor('#4466cc')
      .atmosphereAltitude(0.18)
      .htmlElementsData([])
      .htmlLat((d) => (d as LivePoint).lat)
      .htmlLng((d) => (d as LivePoint).lng)
      .htmlElement((d) => markerElement(d as LivePoint))

    const controls = g.controls() as {
      autoRotate: boolean
      autoRotateSpeed: number
      enablePan: boolean
      enableZoom: boolean
      minDistance: number
      maxDistance: number
      addEventListener: (ev: string, fn: () => void) => void
    }
    controls.autoRotate = true
    controls.autoRotateSpeed = 0.3
    controls.enablePan = false
    controls.enableZoom = true
    controls.minDistance = 101
    controls.maxDistance = 600
    // Nice-to-have: pause autorotate while the user is dragging.
    controls.addEventListener('start', () => {
      controls.autoRotate = false
    })

    g.width(window.innerWidth).height(window.innerHeight)
    globeRef.current = g

    const onResize = () => g.width(window.innerWidth).height(window.innerHeight)
    window.addEventListener('resize', onResize)

    return () => {
      window.removeEventListener('resize', onResize)
      g._destructor()
      globeRef.current = null
    }
  }, [])

  // ── Build a glowing marker DOM node (worldmonitor look) ──────────────────────
  function markerElement(p: LivePoint): HTMLElement {
    const el = document.createElement('div')
    el.className = 'globe-marker'

    const dot = document.createElement('div')
    dot.className = 'globe-dot'

    const ring = document.createElement('div')
    ring.className = 'globe-ring'

    const label = document.createElement('div')
    label.className = 'globe-label'
    label.textContent = `${p.country.toUpperCase()} — ${p.name} · ${p.count.toLocaleString()} msgs`
    label.style.borderColor = p.color

    el.appendChild(ring)
    el.appendChild(dot)
    el.appendChild(label)
    applyMarkerStyle(el, p, false)
    return el
  }

  // ── Push markers into the globe (visible continents only) + pulse styling ────
  useEffect(() => {
    const g = globeRef.current
    if (!g) return
    const visible = points.filter((p) => !hidden.has(p.continentId))
    const maxCount = Math.max(1, ...visible.map((p) => p.count))
    const now = Date.now()

    g.htmlElementsData(visible)
      .htmlElement((d) => {
        const p = d as LivePoint
        const el = markerElement(p)
        applyMarkerStyle(el, p, p.pulseUntil > now, maxCount)
        return el
      })
    // `tick` dep ensures pulses decay back to base styling each second.
  }, [points, hidden, tick])

  // Size + color a marker element by count, with an enlarged/brightened pulse.
  function applyMarkerStyle(el: HTMLElement, p: LivePoint, pulsing: boolean, maxCount = 1) {
    const norm = Math.sqrt(p.count) / Math.sqrt(maxCount)
    let r = MIN_R + (MAX_R - MIN_R) * norm
    let color = p.color
    if (pulsing) {
      r *= 1.6
      color = lighten(p.color, 0.5)
    }
    const size = Math.round(r * 2)

    el.style.position = 'relative'
    el.style.width = '0'
    el.style.height = '0'
    el.style.pointerEvents = 'auto'
    el.style.cursor = 'pointer'
    el.style.zIndex = String(Math.round(p.count))

    const dot = el.querySelector('.globe-dot') as HTMLElement
    dot.style.position = 'absolute'
    dot.style.left = `${-r}px`
    dot.style.top = `${-r}px`
    dot.style.width = `${size}px`
    dot.style.height = `${size}px`
    dot.style.borderRadius = '50%'
    dot.style.background = color
    dot.style.boxShadow = `0 0 ${Math.round(r * 0.9)}px ${Math.round(r * 0.45)}px ${color}88, 0 0 2px ${color}`
    dot.style.border = `1px solid ${lighten(color, 0.4)}`
    dot.style.transition = 'all 0.6s ease'

    const ring = el.querySelector('.globe-ring') as HTMLElement
    const ringSize = size + 8
    ring.style.position = 'absolute'
    ring.style.left = `${-(ringSize / 2)}px`
    ring.style.top = `${-(ringSize / 2)}px`
    ring.style.width = `${ringSize}px`
    ring.style.height = `${ringSize}px`
    ring.style.borderRadius = '50%'
    ring.style.border = `1.5px solid ${color}`
    ring.style.animation = 'globe-pulse 2.4s ease-out infinite'

    const label = el.querySelector('.globe-label') as HTMLElement
    label.style.position = 'absolute'
    label.style.left = `${r + 6}px`
    label.style.top = `${-7}px`
    label.style.whiteSpace = 'nowrap'
    label.style.font = `10px ${MONO}`
    label.style.letterSpacing = '0.04em'
    label.style.color = C.text
    label.style.background = 'rgba(10,10,10,0.82)'
    label.style.border = `1px solid ${p.color}`
    label.style.borderRadius = '2px'
    label.style.padding = '2px 5px'
    label.style.opacity = '0'
    label.style.transition = 'opacity 0.15s'
    label.style.pointerEvents = 'none'

    el.onmouseenter = () => {
      label.style.opacity = '1'
    }
    el.onmouseleave = () => {
      label.style.opacity = '0'
    }
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
  const totalChannels = visiblePts.length
  const totalMsgs = visiblePts.reduce((s, p) => s + p.count, 0)
  const secsAgo = lastUpdate ? Math.max(0, Math.round((Date.now() - lastUpdate) / 1000)) : null
  const windowLabel = WINDOW_OPTIONS.find((w) => w.days === windowDays)?.label ?? 'ALL'
  void tick // keeps secsAgo recomputing every second

  function toggleContinent(id: string) {
    setHidden((cur) => {
      const next = new Set(cur)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
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
        @keyframes globe-pulse {
          0%   { transform: scale(0.6); opacity: 0.9; }
          70%  { transform: scale(1.8); opacity: 0; }
          100% { transform: scale(1.8); opacity: 0; }
        }
        @keyframes live-blink {
          0%, 100% { opacity: 1; }
          50%      { opacity: 0.25; }
        }
      `}</style>

      <div ref={containerRef} style={{ position: 'absolute', inset: 0 }} />

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

      {/* ── Recent activity ticker (bottom-right) ────────────────────────────── */}
      {activity.length > 0 && (
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
                  {a.country ? `${a.country.toUpperCase()} ` : ''}
                  {a.name}
                </span>
                <span style={{ color: C.green }}>+{a.delta}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
