function tokens(text: string): string[] {
  return text.toLowerCase().split(/[^\p{L}\p{N}]+/u).filter((t) => t.length > 0)
}

// Anchor terms: lowercase tokens (len >= 4) present in at least ceil(n/2) queries.
export function subjectAnchors(queries: string[]): string[] {
  const counts = new Map<string, number>()
  for (const q of queries) {
    for (const t of new Set(tokens(q))) {
      if (t.length >= 4) counts.set(t, (counts.get(t) || 0) + 1)
    }
  }
  const need = Math.ceil(queries.length / 2)
  return queries.length === 0 ? [] : [...counts].filter(([, c]) => c >= need).map(([t]) => t)
}

export function mentionsAnchor(text: string, anchors: string[]): boolean {
  if (anchors.length === 0) return false
  const set = new Set(tokens(text))
  return anchors.some((a) => set.has(a))
}
