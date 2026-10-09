import type { ChannelFilters } from './api'

// A row in the rules table. Union of the per-channel keep_regex / drop_regex /
// incident_only / drop_authors maps, edited together and serialized back on save.
export type FilterRule = { id: string; keep: string; drop: string; incident: string; dropAuthors: string }

const splitList = (s: string) => s.split(',').map((a) => a.trim()).filter(Boolean)

export function rulesFromCfg(cfg: ChannelFilters): FilterRule[] {
  const ids = new Set<string>([
    ...Object.keys(cfg.keep_regex ?? {}),
    ...Object.keys(cfg.drop_regex ?? {}),
    ...Object.keys(cfg.incident_only ?? {}),
    ...Object.keys(cfg.drop_authors ?? {}),
  ])
  return Array.from(ids).map((id) => ({
    id,
    keep: cfg.keep_regex?.[id] ?? '',
    drop: cfg.drop_regex?.[id] ?? '',
    incident: (cfg.incident_only?.[id] ?? []).join(', '),
    dropAuthors: (cfg.drop_authors?.[id] ?? []).join(', '),
  }))
}

// Never emits `names`: that key is server-derived (GET only).
export function cfgFromState(ignore: string[], rules: FilterRule[]): ChannelFilters {
  const keep_regex: Record<string, string> = {}
  const drop_regex: Record<string, string> = {}
  const incident_only: Record<string, string[]> = {}
  const drop_authors: Record<string, string[]> = {}
  for (const r of rules) {
    const id = r.id.trim()
    if (!id) continue
    if (r.keep.trim()) keep_regex[id] = r.keep.trim()
    if (r.drop.trim()) drop_regex[id] = r.drop.trim()
    const authors = splitList(r.incident)
    if (authors.length) incident_only[id] = authors
    const dropped = splitList(r.dropAuthors)
    if (dropped.length) drop_authors[id] = dropped
  }
  return { ignore, keep_regex, drop_regex, incident_only, drop_authors }
}

// Channel name for a chip: the channels list, else the server-resolved names
// map, else a placeholder (the id goes in the tooltip).
export function channelLabel(
  id: string,
  channels: { channel_id: string; name?: string }[],
  names: Record<string, string> | undefined,
): string {
  return channels.find((c) => c.channel_id === id)?.name || names?.[id] || 'unknown channel'
}
