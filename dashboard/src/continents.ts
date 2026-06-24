import type { ContinentCfg } from './api'
import { COUNTRIES } from './countries-data'

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

// applyGroupNames replaces unresolved Slack usergroup ids in a message body
// (e.g. "@S01TMG8Q65R") with their configured name ("@payments-geeks") from
// cfg.groups. No-op when no groups are configured.
export function applyGroupNames(text: string, cfg: ContinentCfg): string {
  const groups = cfg.groups
  if (!groups) return text
  let out = text
  for (const [gid, name] of Object.entries(groups)) {
    out = out.split('@' + gid).join('@' + name).split(gid).join('@' + name)
  }
  return out
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

// ----- Channel -> country assignment ("every channel = one country") -----

// Default country pool per continent id, ordered big -> small (by land area) so
// the highest-volume channel in a continent lands on the largest country.
// Lists are disjoint so a country is never claimed by two continents.
export const DEFAULT_CONTINENT_COUNTRIES: Record<string, string[]> = {
  // payments -> Asia
  core: ['CN','IN','KZ','SA','IR','MN','ID','PK','TR','MM','AF','YE','TH','TM','UZ','JP','VN','MY','PH','KR'],
  // payments-partner -> Europe
  partners: ['UA','FR','ES','SE','DE','FI','NO','PL','IT','GB','RO','BY','GR','BG','IS','PT','CZ','IE','AT','RS'],
  // other -> Africa
  other: ['DZ','CD','SD','LY','TD','NE','AO','ML','ZA','ET','NG','MR','EG','TZ','MZ','NA','ZM','MA','SO','KE'],
}

// Global fallback pool ordered by area, for unknown continents or overflow when a
// continent has more channels than its country list. Used countries are skipped.
const GLOBAL_BY_AREA = [
  'RU','CA','US','CN','BR','AU','IN','AR','KZ','DZ','CD','SA','MX','ID','SD','LY','IR','MN','PE',
  'TD','NE','AO','ML','ZA','CO','ET','BO','MR','EG','TZ','NG','VE','NA','MZ','PK','TR','CL','ZM',
  'MM','AF','SO','CF','UA','MG','BW','KE','FR','YE','TH','ES','TM','CM','PG','SE','UZ','MA','IQ',
  'PY','ZW','JP','DE','CG','FI','VN','MY','NO','PL','IT','PH','EC','BF','NZ','GA','GN','GB','UG','GH',
]

export type CountryAssignment = { iso: string; name: string; lat: number; lon: number }

// assignCountries maps each channel to a unique real country. Channels are grouped
// by continent, sorted by count desc, and assigned to that continent's ordered
// country list (biggest channel -> biggest country); overflow draws from the
// global-by-area pool. Returns channelId -> country (skips channels if the world
// runs out of countries, which won't happen for <168 channels).
export function assignCountries(
  channels: { channel_id: string; count: number }[],
  cfg: ContinentCfg,
): Record<string, CountryAssignment> {
  const byContinent: Record<string, { channel_id: string; count: number }[]> = {}
  for (const ch of channels) {
    const cid = continentOf(ch.channel_id, cfg) || '__none'
    ;(byContinent[cid] ||= []).push(ch)
  }
  const used = new Set<string>()
  const result: Record<string, CountryAssignment> = {}
  const take = (pool: string[]): string | null => {
    for (const iso of pool) {
      if (!used.has(iso) && COUNTRIES[iso]) {
        used.add(iso)
        return iso
      }
    }
    return null
  }
  for (const [cid, list] of Object.entries(byContinent)) {
    list.sort((a, b) => b.count - a.count)
    const primary = DEFAULT_CONTINENT_COUNTRIES[cid] || []
    for (const ch of list) {
      const iso = take(primary) || take(GLOBAL_BY_AREA)
      if (!iso) continue
      const c = COUNTRIES[iso]
      result[ch.channel_id] = { iso, name: c.name, lat: c.lat, lon: c.lon }
    }
  }
  return result
}
