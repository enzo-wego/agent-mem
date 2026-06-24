import { useEffect, useMemo, useRef, useState } from 'react'
import Globe, { type GlobeInstance } from 'globe.gl'
import { fetchChannels, fetchContinents, type ChannelCount, type ContinentCfg } from '../api'
import { continentOf, nameOf, placement } from '../continents'

interface LivePoint {
  channelId: string
  name: string
  count: number
  lat: number
  lng: number
  color: string
  continentId: string
  // pulseUntil: epoch ms until which this point is highlighted (count increased).
  pulseUntil: number
}

interface ActivityEntry {
  name: string
  delta: number
  at: number
}

const REFRESH_OPTIONS = [5, 10, 30] as const
const PULSE_MS = 2000

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
  const centerById = new Map(cfg.continents.map((c) => [c.id, c.center]))
  const colorById = new Map(cfg.continents.map((c) => [c.id, c.color]))
  const pts: LivePoint[] = []
  for (const ch of channels) {
    const cid = continentOf(ch.channel_id, cfg)
    const center = centerById.get(cid) ?? [0, 0]
    const [lat, lng] = placement(ch.channel_id, center as [number, number])
    pts.push({
      channelId: ch.channel_id,
      name: nameOf(ch.channel_id, cfg),
      count: ch.count,
      lat,
      lng,
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
  const [lastUpdate, setLastUpdate] = useState<number>(0)
  const [tick, setTick] = useState(0)
  const [error, setError] = useState('')
  const [activity, setActivity] = useState<ActivityEntry[]>([])

  // Refs mirror state for use inside the polling closure without re-subscribing.
  const prevCountsRef = useRef<Map<string, number>>(new Map())
  const cfgRef = useRef<ContinentCfg | null>(null)
  const inFlightRef = useRef(false)
  const intervalRef = useRef(10)
  intervalRef.current = interval

  // ── Single channel poll (guarded against overlap) ───────────────────────────
  useEffect(() => {
    let cancelled = false

    async function poll() {
      if (cancelled || inFlightRef.current || !cfgRef.current) return
      inFlightRef.current = true
      try {
        const channels = await fetchChannels()
        if (cancelled) return
        const cfgNow = cfgRef.current
        if (!cfgNow) return

        const prev = prevCountsRef.current
        const now = Date.now()
        const pulse = new Map<string, number>()
        const increases: ActivityEntry[] = []
        for (const ch of channels || []) {
          const before = prev.get(ch.channel_id)
          if (before !== undefined && ch.count > before) {
            pulse.set(ch.channel_id, now + PULSE_MS)
            increases.push({ name: nameOf(ch.channel_id, cfgNow), delta: ch.count - before, at: now })
          }
        }
        // Record current counts for the next diff.
        const nextCounts = new Map<string, number>()
        for (const ch of channels || []) nextCounts.set(ch.channel_id, ch.count)
        prevCountsRef.current = nextCounts

        setPoints(buildPoints(channels || [], cfgNow, pulse))
        setLastUpdate(now)
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

    // Re-created whenever the selected refresh rate changes (effect dep).
    const id = window.setInterval(() => {
      if (document.hidden) return
      void poll()
    }, intervalRef.current * 1000)

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
    // Re-run when interval changes so the timer uses the new rate.
  }, [interval])

  // ── Ticker: drives "updated Xs ago" + decays pulses ─────────────────────────
  useEffect(() => {
    const id = window.setInterval(() => setTick((t) => t + 1), 1000)
    return () => window.clearInterval(id)
  }, [])

  // ── Create the globe once ───────────────────────────────────────────────────
  useEffect(() => {
    if (!containerRef.current || globeRef.current) return
    const g = new Globe(containerRef.current)
      .backgroundColor('#0d1117')
      .showAtmosphere(true)
      .atmosphereColor('#3fb950')
      .atmosphereAltitude(0.18)
    const mat = g.globeMaterial() as {
      color: { set: (c: string) => void }
      emissive: { set: (c: string) => void }
      emissiveIntensity: number
      shininess: number
    }
    mat.color.set('#0f1620')
    mat.emissive.set('#08160d')
    mat.emissiveIntensity = 0.12
    mat.shininess = 4
    const controls = g.controls() as { autoRotate: boolean; autoRotateSpeed: number }
    controls.autoRotate = true
    controls.autoRotateSpeed = 0.4
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

  // ── Push point data into the globe (visible continents only) + pulse styling ─
  useEffect(() => {
    const g = globeRef.current
    if (!g) return
    const visible = points.filter((p) => !hidden.has(p.continentId))
    const maxCount = Math.max(1, ...visible.map((p) => p.count))
    const norm = (c: number) => Math.sqrt(c) / Math.sqrt(maxCount)
    const now = Date.now()

    g.pointsData(visible)
      .pointLat((d) => (d as LivePoint).lat)
      .pointLng((d) => (d as LivePoint).lng)
      .pointColor((d) => {
        const p = d as LivePoint
        return p.pulseUntil > now ? lighten(p.color, 0.6) : p.color
      })
      .pointAltitude((d) => {
        const p = d as LivePoint
        const base = 0.01 + 0.18 * norm(p.count)
        return p.pulseUntil > now ? base + 0.22 : base
      })
      .pointRadius((d) => {
        const p = d as LivePoint
        const base = 0.25 + 0.7 * norm(p.count)
        return p.pulseUntil > now ? base * 2 : base
      })
      .pointLabel((d) => {
        const p = d as LivePoint
        return `${p.name} — ${p.count} msgs`
      })
    g.pointsTransitionDuration(800)
    // `tick` + `points` dependencies ensure pulses decay back to base each second.
  }, [points, hidden, tick])

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
  void tick // keeps secsAgo recomputing every second

  function toggleContinent(id: string) {
    setHidden((cur) => {
      const next = new Set(cur)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  return (
    <div className="fixed inset-0 w-screen h-screen overflow-hidden bg-[#0d1117] text-gray-100">
      <div ref={containerRef} className="absolute inset-0" />

      {/* Control panel */}
      <div className="absolute top-4 left-4 w-72 max-h-[calc(100vh-2rem)] overflow-y-auto rounded-lg bg-black/55 backdrop-blur border border-white/10 p-4 text-xs space-y-3">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <span className="relative flex h-2.5 w-2.5">
              <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-75" />
              <span className="relative inline-flex rounded-full h-2.5 w-2.5 bg-green-500" />
            </span>
            <span className="font-semibold tracking-wide text-sm">LIVE</span>
          </div>
          <span className="text-gray-400">
            {secsAgo === null ? 'connecting…' : `updated ${secsAgo}s ago`}
          </span>
        </div>

        {error && <p className="text-red-400">{error}</p>}

        {/* Totals */}
        <div className="grid grid-cols-2 gap-2 text-center">
          <div className="rounded bg-white/5 py-2">
            <div className="text-base font-semibold">{totalChannels}</div>
            <div className="text-[10px] uppercase text-gray-400 tracking-wide">channels</div>
          </div>
          <div className="rounded bg-white/5 py-2">
            <div className="text-base font-semibold">{totalMsgs.toLocaleString()}</div>
            <div className="text-[10px] uppercase text-gray-400 tracking-wide">messages</div>
          </div>
        </div>

        {/* Refresh interval */}
        <div>
          <div className="text-[10px] uppercase text-gray-400 tracking-wide mb-1">Refresh</div>
          <div className="flex gap-1">
            {REFRESH_OPTIONS.map((s) => (
              <button
                key={s}
                onClick={() => setIntervalSec(s)}
                className={`flex-1 py-1 rounded border transition-colors ${
                  interval === s
                    ? 'border-green-500 bg-green-500/20 text-green-300'
                    : 'border-white/10 text-gray-400 hover:text-gray-200'
                }`}
              >
                {s}s
              </button>
            ))}
          </div>
        </div>

        {/* Continent layer toggles */}
        <div>
          <div className="text-[10px] uppercase text-gray-400 tracking-wide mb-1">Continents</div>
          <div className="space-y-1">
            {legend.map((c) => {
              const on = !hidden.has(c.id)
              return (
                <button
                  key={c.id}
                  onClick={() => toggleContinent(c.id)}
                  className={`w-full flex items-center gap-2 rounded px-2 py-1 text-left transition-colors ${
                    on ? 'bg-white/5 hover:bg-white/10' : 'opacity-40 hover:opacity-70'
                  }`}
                >
                  <span
                    className="inline-block w-3 h-3 rounded-full shrink-0"
                    style={{ backgroundColor: c.color }}
                  />
                  <span className="flex-1 truncate">{c.label}</span>
                  <span className="text-gray-400">{c.channelCount}</span>
                  <span className="text-gray-500 tabular-nums">{c.msgs.toLocaleString()}</span>
                </button>
              )
            })}
          </div>
        </div>

        {/* Recent activity */}
        {activity.length > 0 && (
          <div>
            <div className="text-[10px] uppercase text-gray-400 tracking-wide mb-1">Recent activity</div>
            <div className="space-y-0.5">
              {activity.map((a, i) => (
                <div key={`${a.name}-${a.at}-${i}`} className="flex items-center gap-1 text-green-300">
                  <span>▲</span>
                  <span className="flex-1 truncate text-gray-200">{a.name}</span>
                  <span>+{a.delta}</span>
                </div>
              ))}
            </div>
          </div>
        )}

        <a href="/" className="block text-center text-gray-400 hover:text-gray-200 pt-1">
          ← back to dashboard
        </a>
      </div>
    </div>
  )
}
