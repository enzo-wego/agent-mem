import { useEffect, useRef, useState } from 'react'
import Globe, { type GlobeInstance } from 'globe.gl'
import { fetchChannels, fetchContinents, type ChannelCount, type ContinentCfg } from '../api'
import { continentOf, nameOf, placement } from '../continents'

interface ChannelPoint {
  channelId: string
  name: string
  count: number
  lat: number
  lng: number
  color: string
  continentId: string
}

function buildPoints(channels: ChannelCount[], cfg: ContinentCfg): ChannelPoint[] {
  const centerById = new Map(cfg.continents.map((c) => [c.id, c.center]))
  const colorById = new Map(cfg.continents.map((c) => [c.id, c.color]))
  const pts: ChannelPoint[] = []
  for (const ch of channels) {
    const cid = continentOf(ch.channel_id, cfg, ch.name)
    const center = centerById.get(cid) ?? [0, 0]
    const [lat, lng] = placement(ch.channel_id, center as [number, number])
    pts.push({
      channelId: ch.channel_id,
      name: nameOf(ch.channel_id, cfg, ch.name),
      count: ch.count,
      lat,
      lng,
      color: colorById.get(cid) ?? '#8b949e',
      continentId: cid,
    })
  }
  return pts
}

export function GlobePage() {
  const containerRef = useRef<HTMLDivElement>(null)
  const globeRef = useRef<GlobeInstance | null>(null)
  const [cfg, setCfg] = useState<ContinentCfg | null>(null)
  const [channels, setChannels] = useState<ChannelCount[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)

  // Load data once on mount.
  useEffect(() => {
    let cancelled = false
    Promise.all([fetchChannels(), fetchContinents()])
      .then(([ch, c]) => {
        if (cancelled) return
        setChannels(ch || [])
        setCfg(c)
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : 'Failed to load')
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  // Create / update the globe when data or container is ready.
  useEffect(() => {
    if (!containerRef.current || !cfg) return

    const points = buildPoints(channels, cfg)
    const maxCount = Math.max(1, ...points.map((p) => p.count))

    if (!globeRef.current) {
      const g = new Globe(containerRef.current)
        .backgroundColor('#0d1117')
        .showAtmosphere(true)
        .atmosphereColor('#3fb950')
        .atmosphereAltitude(0.18)
      // Plain colored globe — NO remote texture (offline/behind-auth safe).
      const mat = g.globeMaterial() as {
        color: { set: (c: string) => void }
        emissive: { set: (c: string) => void }
        emissiveIntensity: number
        shininess: number
      }
      mat.color.set('#161b22')
      mat.emissive.set('#0a1f12')
      mat.emissiveIntensity = 0.1
      mat.shininess = 5
      globeRef.current = g
    }

    const g = globeRef.current
    g.width(containerRef.current.clientWidth)
    g.height(700)
    g.pointsData(points)
      .pointLat((d) => (d as ChannelPoint).lat)
      .pointLng((d) => (d as ChannelPoint).lng)
      .pointColor((d) => (d as ChannelPoint).color)
      .pointAltitude((d) => 0.01 + 0.18 * (Math.sqrt((d as ChannelPoint).count) / Math.sqrt(maxCount)))
      .pointRadius((d) => 0.25 + 0.7 * (Math.sqrt((d as ChannelPoint).count) / Math.sqrt(maxCount)))
      .pointLabel((d) => {
        const p = d as ChannelPoint
        return `${p.name} — ${p.count} msgs`
      })

    // Auto-rotate for the worldmonitor.app vibe.
    const controls = g.controls() as { autoRotate: boolean; autoRotateSpeed: number }
    controls.autoRotate = true
    controls.autoRotateSpeed = 0.4
  }, [channels, cfg])

  // Clean up globe instance on unmount.
  useEffect(() => {
    return () => {
      if (globeRef.current) {
        globeRef.current._destructor()
        globeRef.current = null
      }
    }
  }, [])

  // Legend: per-continent label, color and channel count.
  const legend = cfg
    ? cfg.continents.map((c) => ({
        ...c,
        channelCount: channels.filter((ch) => continentOf(ch.channel_id, cfg) === c.id).length,
      }))
    : []
  const totalChannels = channels.length
  const totalMsgs = channels.reduce((sum, c) => sum + c.count, 0)

  return (
    <div>
      <div className="flex items-center justify-between mb-3">
        <div>
          <h2 className="text-lg font-semibold">Globe</h2>
          <p className="text-sm text-gray-500 dark:text-gray-400">
            Slack channels as nations on a globe, grouped by continent.
          </p>
        </div>
        <div className="text-right text-sm text-gray-500 dark:text-gray-400">
          <div>{totalChannels} channels</div>
          <div>{totalMsgs.toLocaleString()} messages</div>
        </div>
      </div>

      {error && <p className="text-red-500 text-sm mb-3">{error}</p>}
      {loading && <p className="text-sm text-gray-500 dark:text-gray-400">Loading…</p>}

      <div className="relative rounded-lg overflow-hidden border border-gray-200 dark:border-gray-700 bg-[#0d1117]">
        <div ref={containerRef} className="w-full" style={{ height: 700 }} />
        {legend.length > 0 && (
          <div className="absolute top-3 left-3 bg-black/50 backdrop-blur rounded-md p-3 text-xs text-gray-100 space-y-1.5">
            <div className="font-semibold text-gray-300 mb-1">Continents</div>
            {legend.map((c) => (
              <div key={c.id} className="flex items-center gap-2">
                <span className="inline-block w-3 h-3 rounded-full" style={{ backgroundColor: c.color }} />
                <span className="flex-1">{c.label}</span>
                <span className="text-gray-400">{c.channelCount}</span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}
