import type { ContinentCfg } from './api'

// nameOf resolves a channel's display name from the config, falling back to the
// channel id when no name is configured.
export function nameOf(channelId: string, cfg: ContinentCfg): string {
  return cfg.names[channelId] || channelId
}

// continentOf classifies a channel into a continent id. An explicit override
// wins; otherwise the first continent whose `match` list contains a prefix of
// the channel name (where "*" always matches) is used. Returns '' if nothing
// matches (no catch-all configured).
export function continentOf(channelId: string, cfg: ContinentCfg): string {
  const override = cfg.overrides[channelId]
  if (override) return override
  const name = nameOf(channelId, cfg)
  for (const c of cfg.continents) {
    for (const m of c.match) {
      if (m === '*' || name.startsWith(m)) return c.id
    }
  }
  return ''
}

// stringHash is a stable, deterministic 32-bit string hash (djb2). Used to place
// channels at a fixed spot around their continent center.
export function stringHash(s: string): number {
  let h = 5381
  for (let i = 0; i < s.length; i++) {
    h = ((h << 5) + h + s.charCodeAt(i)) | 0
  }
  return h >>> 0
}

// placement returns a deterministic [lat, lon] for a channel, clustered within a
// bounded spread (~18°) around its continent center. Same id always lands in the
// same spot.
export function placement(
  channelId: string,
  center: [number, number],
): [number, number] {
  const h = stringHash(channelId)
  // Derive an angle (0..2π) and a radius (0..~9°) from independent bits of the hash.
  const angle = ((h % 360) * Math.PI) / 180
  const radius = ((Math.floor(h / 360) % 1000) / 1000) * 9
  const [lat, lon] = center
  const newLat = Math.max(-85, Math.min(85, lat + radius * Math.sin(angle)))
  const newLon = lon + radius * Math.cos(angle)
  return [newLat, newLon]
}
