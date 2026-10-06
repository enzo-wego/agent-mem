export type ResolveLink = { kind: string; tag?: string; topic?: string; why?: string; confidence?: number }

type Chip = { label: string; title?: string }

// linkChips turns the direct-seed link kinds of a resolve artifact into UI
// chips (table order) plus the SAME_TOPIC judge reason, if any.
export function linkChips(links: ResolveLink[] | undefined): { chips: Chip[]; why?: string } {
  const chips: Chip[] = []
  if (!links || links.length === 0) return { chips }
  const byKind = new Map<string, ResolveLink>()
  for (const l of links) if (!byKind.has(l.kind)) byKind.set(l.kind, l)

  if (byKind.has('REFERENCES') || byKind.has('REFERS_TO')) chips.push({ label: 'reference' })
  if (byKind.has('THREAD')) chips.push({ label: 'same thread' })
  const topic = byKind.get('SAME_TOPIC')
  let why: string | undefined
  if (topic) {
    const label = typeof topic.confidence === 'number' ? `same topic ${topic.confidence.toFixed(2)}` : 'same topic'
    chips.push(topic.topic ? { label, title: topic.topic } : { label })
    if (topic.why) why = topic.why
  }
  if (byKind.has('PART_OF')) chips.push({ label: 'epic' })
  const known = new Set(['REFERENCES', 'REFERS_TO', 'THREAD', 'SAME_TOPIC', 'PART_OF'])
  for (const kind of [...byKind.keys()].filter((k) => !known.has(k)).sort()) {
    chips.push({ label: kind.toLowerCase().replaceAll('_', ' ') })
  }
  return why ? { chips, why } : { chips }
}
