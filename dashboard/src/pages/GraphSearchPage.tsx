import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  fetchClusterSummary,
  graphNeighborsCards,
  graphResolve,
  graphSearchHybrid,
  graphSubjectQueries,
  parseGraphSeed,
  parseSlackLink,
  type ClusterSummary,
  type GraphNeighbor,
  type PRRef,
  type HybridSearchResult,
} from '../api'

// ── palette (copied from LiveGlobe.tsx:41-54; panel2/border2/grid and the per-
// resource colours come from the approved mockup's dark tokens) ───────────────
const C = {
  bg: '#0a0a0a',
  panel: '#141414',
  panel2: '#1b1b1b',
  border: '#2a2a2a',
  border2: '#383838',
  text: '#e8e8e8',
  dim: '#888888',
  dim2: '#5a5a5a',
  green: '#44ff88',
  amber: '#ffaa00',
  red: '#ff4444',
  blue: '#4da3ff',
  purple: '#c084fc',
  grid: '#1c1c1c',
} as const

const MONO = 'ui-monospace, "SF Mono", Menlo, monospace'

// ── resource groups, in band-slot / rail order ───────────────────────────────
type Group = 'slack' | 'pr' | 'jira' | 'doc' | 'alert'
const GROUPS: { key: Group; label: string; icon: string; color: string }[] = [
  { key: 'slack', label: 'Slack threads', icon: '#', color: C.green },
  { key: 'pr', label: 'Pull requests', icon: '⎇', color: C.purple },
  { key: 'jira', label: 'Jira', icon: '◆', color: C.blue },
  { key: 'doc', label: 'Docs', icon: '▤', color: C.amber },
  { key: 'alert', label: 'Alerts', icon: '⚠', color: C.red },
]
const GROUP_OF: Record<string, Group> = {
  slack: 'slack',
  slack_thread: 'slack',
  gh_pr: 'pr',
  jira: 'jira',
  cf: 'doc',
  cf_page: 'doc',
  gws: 'doc',
  gws_doc: 'doc',
  gdoc: 'doc',
  wegohub: 'doc',
  claude_artifact: 'doc',
  pagerduty: 'alert',
  sentry: 'alert',
  datadog: 'alert',
}
const GROUP_META = Object.fromEntries(GROUPS.map((g, i) => [g.key, { ...g, slot: i }])) as Record<
  Group,
  (typeof GROUPS)[number] & { slot: number }
>

// ── evidence ─────────────────────────────────────────────────────────────────
type Ev = 'direct' | 'judge' | 'similar' | 'hop2' | 'subject' | 'keyword' | 'semantic' | 'both'
const EV_META: Record<Ev, { label: string; glyph: string; color: string; rank: number }> = {
  direct: { label: 'direct link', glyph: '━', color: C.text, rank: 0 },
  judge: { label: 'judge: same topic', glyph: '┅', color: C.blue, rank: 1 },
  similar: { label: 'similar wording', glyph: '┈', color: C.amber, rank: 2 },
  hop2: { label: 'two hops', glyph: '·', color: C.dim, rank: 3 },
  subject: { label: 'same subject', glyph: '≋', color: C.purple, rank: 4 },
  both: { label: 'keyword + semantic', glyph: '✦', color: C.green, rank: 0 },
  keyword: { label: 'keyword', glyph: '⌕', color: C.text, rank: 1 },
  semantic: { label: 'semantic', glyph: '≈', color: C.blue, rank: 2 },
}
const SEED_FILTERS: Ev[] = ['direct', 'judge', 'similar', 'hop2']
const FREE_FILTERS: Ev[] = ['keyword', 'semantic', 'both']

interface Item {
  key: string // sync id: thread root id for Slack, node id otherwise
  group: Group
  title: string
  url: string
  overview: string
  channel: string
  rootAuthor: string
  participants: string[]
  participantCount: number
  msgCount: number
  first: number
  last: number
  ev: Ev
  why: string
  jiraKey: string
  prCount: number // Jira only: linked PRs (full count)
  prs: PRRef[] // Jira only: first 20
  idx: number // server order (free-text mode)
}

interface View {
  mode: 'seed' | 'free'
  q: string
  seed?: { id: string; type: string; title: string }
  items: Item[]
  nodeRoot: Record<string, string> // any Slack row node id → its thread root id
  summary: ClusterSummary | 'loading' | 'none'
  notice: string
  semErr: boolean
  banner?: { label: string; value: string }
  seedPRs?: { pr_count: number; prs: PRRef[] }
}

type Status = { kind: 'idle' } | { kind: 'loading' } | { kind: 'error'; msg: string } | { kind: 'ready'; view: View }

// ── helpers ──────────────────────────────────────────────────────────────────
const DAY = 864e5
const PPD = 26 // px per day
const WEEK_H = 7 * PPD
const QUIET_H = 28
const MAX_WEEKS = 26

function ago(ms: number): string {
  const d = Math.floor((Date.now() - ms) / DAY)
  return d <= 0 ? 'today' : `${d}d ago`
}
const isFresh = (ms: number) => Date.now() - ms <= 7 * DAY
const fmtDay = (ms: number) => new Date(ms).toLocaleDateString('en-GB', { day: 'numeric', month: 'short' })
const fmtDT = (ms: number) =>
  new Date(ms).toLocaleString('en-GB', { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' })

function monday(ms: number): number {
  const d = new Date(ms)
  d.setHours(0, 0, 0, 0)
  d.setDate(d.getDate() - ((d.getDay() + 6) % 7))
  return d.getTime()
}
function addWeeks(ms: number, n: number): number {
  const d = new Date(ms)
  d.setDate(d.getDate() + 7 * n)
  return d.getTime()
}

function slackRootOf(nodeId: string, threadTs?: string, threadRoot?: string): string {
  if (threadRoot) return threadRoot
  const parts = nodeId.split(':')
  if (parts.length !== 3) return nodeId
  return `slack:${parts[1]}:${threadTs || parts[2]}`
}

function rowEv(n: GraphNeighbor): Ev | null {
  if (n.hop >= 2) return 'hop2'
  switch (n.edge.kind) {
    case 'REFERENCES':
    case 'REFERS_TO':
      return 'direct'
    case 'SAME_TOPIC':
      return 'judge'
    case 'SIMILAR':
      return 'similar'
    default:
      return null // THREAD: the thread's other rows decide
  }
}

function rowWhy(n: GraphNeighbor, ev: Ev): string {
  switch (ev) {
    case 'direct':
      return 'Linked directly: an explicit reference connects it to the opened item.'
    case 'judge': {
      const pct = n.edge.confidence ? ` (${Math.round(n.edge.confidence * 100)}%)` : ''
      return `Judged the same topic${pct}${n.edge.why ? `: ${n.edge.why}` : '.'}`
    }
    case 'similar': {
      const sc = n.edge.score ? ` (score ${n.edge.score.toFixed(2)})` : ''
      const v = n.edge.verdict ? `; verdict: ${n.edge.verdict}${n.edge.verdict_why ? ` — ${n.edge.verdict_why}` : ''}` : ''
      return `Similar wording${sc}${v}.`
    }
    default:
      return n.node.via ? `Two hops away, reached through “${n.node.via}”.` : 'Two hops away.'
  }
}

function buildSeedItems(rows: GraphNeighbor[], seedRoot: string): { items: Item[]; nodeRoot: Record<string, string> } {
  const items = new Map<string, Item & { rank: number }>()
  const nodeRoot: Record<string, string> = {}
  for (const row of rows) {
    const nd = row.node
    const group = GROUP_OF[nd.type]
    if (!group) continue
    const slack = group === 'slack'
    const key = slack ? slackRootOf(nd.node_id, nd.thread_ts, nd.thread_root) : nd.node_id
    if (slack) {
      nodeRoot[nd.node_id] = key
      if (key === seedRoot) continue
    }
    const first = slack ? nd.first_ts_ms || nd.ts_ms || 0 : nd.ts_ms || 0
    const last = slack ? nd.last_ts_ms || nd.ts_ms || 0 : nd.ts_ms || 0
    const ev = rowEv(row)
    let it = items.get(key)
    if (!it) {
      it = {
        key,
        group,
        title: nd.title || '',
        url: nd.url || '',
        overview: nd.overview || '',
        channel: nd.channel || '',
        rootAuthor: nd.root_author || '',
        participants: nd.participants || [],
        participantCount: nd.participant_count || 0,
        msgCount: nd.msg_count || 0,
        first,
        last,
        ev: 'hop2',
        why: '',
        jiraKey: nd.type === 'jira' ? nd.node_id.replace(/^jira:/, '') : '',
        prCount: nd.pr_count || 0,
        prs: nd.prs || [],
        idx: items.size,
        rank: 99,
      }
      items.set(key, it)
    } else {
      if (nd.node_id === key && nd.title) it.title = nd.title
      it.overview = it.overview || nd.overview || ''
      it.channel = it.channel || nd.channel || ''
      it.first = it.first && first ? Math.min(it.first, first) : it.first || first
      it.last = Math.max(it.last, last)
    }
    if (ev && EV_META[ev].rank < it.rank) {
      it.rank = EV_META[ev].rank
      it.ev = ev
      it.why = rowWhy(row, ev)
    }
  }
  const out = [...items.values()].map((it) => {
    if (it.rank === 99) {
      it.why = 'Only reached through the opened thread’s replies (two hops).'
    }
    const { rank, ...rest } = it
    void rank
    return rest as Item
  })
  return { items: out, nodeRoot }
}

function buildFreeItems(results: HybridSearchResult[]): Item[] {
  const out: Item[] = []
  for (const r of results) {
    const group = GROUP_OF[r.type]
    if (!group) continue
    const slack = group === 'slack'
    const created = Date.parse(r.created_at) || 0
    const kw = r.match?.includes('keyword')
    const sem = r.match?.includes('semantic')
    const ev: Ev = kw && sem ? 'both' : sem ? 'semantic' : 'keyword'
    out.push({
      key: slack ? slackRootOf(r.node_id, undefined, r.thread_root) : r.node_id,
      group,
      title: r.title || '',
      url: r.url || '',
      overview: r.summary || '',
      channel: r.channel || '',
      rootAuthor: r.root_author || '',
      participants: r.participants || [],
      participantCount: r.participant_count || 0,
      msgCount: r.msg_count || 0,
      first: r.first_ts_ms || created,
      last: r.last_ts_ms || created,
      ev,
      why:
        ev === 'both'
          ? 'Matched the query words and is semantically close.'
          : ev === 'keyword'
            ? 'Contains the query words.'
            : 'Semantically close to the query.',
      jiraKey: r.type === 'jira' ? r.node_id.replace(/^jira:/, '') : '',
      prCount: r.pr_count || 0,
      prs: r.prs || [],
      idx: out.length,
    })
  }
  return out
}

// Slack thread section order: seed mode ranks by evidence class then latest
// reply; free-text keeps the server's (hybrid score) order.
function threadOrder(items: Item[], mode: View['mode']): Item[] {
  const s = items.filter((i) => i.group === 'slack')
  if (mode === 'free') return s.sort((a, b) => a.idx - b.idx)
  return s.sort((a, b) => EV_META[a.ev].rank - EV_META[b.ev].rank || b.last - a.last)
}

function pickBanner(results: HybridSearchResult[]): View['banner'] {
  const top = results.find((r) => GROUP_OF[r.type] && GROUP_OF[r.type] !== 'slack')
  if (!top) return undefined
  const value = top.url || (top.type === 'jira' ? top.node_id.replace(/^jira:/, '') : '')
  if (!value) return undefined
  return { label: top.title || value, value }
}

// ── story geometry ───────────────────────────────────────────────────────────
interface Seg {
  kind: 'week' | 'quiet'
  y: number
  h: number
  start: number // week start (week) 
  end: number // next week start (week)
  n: number // quiet weeks
}
interface Story {
  segs: Seg[]
  shown: Item[]
  olderCount: number
  cutoff: number
  timeH: number
  yt: Record<string, number> // bar top / card anchor
  yb: Record<string, number> // bar bottom
}

function layoutStory(items: Item[], showAll: boolean): Story {
  const dated = items.filter((i) => i.last > 0)
  const undated = items.length - dated.length
  if (dated.length === 0) return { segs: [], shown: [], olderCount: undated, cutoff: 0, timeH: 0, yt: {}, yb: {} }
  const newestWeek = monday(Math.max(...dated.map((i) => i.last)))
  const capStart = addWeeks(newestWeek, -(MAX_WEEKS - 1))
  const shown = showAll ? dated : dated.filter((i) => i.last >= capStart)
  const olderCount = dated.length - shown.length + undated
  if (shown.length === 0) return { segs: [], shown: [], olderCount, cutoff: capStart, timeH: 0, yt: {}, yb: {} }
  const oldestWeek = Math.max(monday(Math.min(...shown.map((i) => i.first || i.last))), showAll ? 0 : capStart)
  const weeks: number[] = []
  for (let w = newestWeek; w >= oldestWeek; w = addWeeks(w, -1)) weeks.push(w)
  const occupied = new Set<number>()
  for (const it of shown) {
    const lo = Math.max(monday(it.first || it.last), oldestWeek)
    for (let w = monday(it.last); w >= lo; w = addWeeks(w, -1)) occupied.add(w)
  }
  const segs: Seg[] = []
  const weekSeg = new Map<number, Seg>()
  let y = 0
  for (let i = 0; i < weeks.length; ) {
    if (occupied.has(weeks[i])) {
      const s: Seg = { kind: 'week', y, h: WEEK_H, start: weeks[i], end: addWeeks(weeks[i], 1), n: 1 }
      segs.push(s)
      weekSeg.set(weeks[i], s)
      y += WEEK_H
      i++
      continue
    }
    let j = i
    while (j < weeks.length && !occupied.has(weeks[j])) j++
    const run = j - i
    if (run >= 2) {
      segs.push({ kind: 'quiet', y, h: QUIET_H, start: weeks[j - 1], end: weeks[i], n: run })
      y += QUIET_H
    } else {
      const s: Seg = { kind: 'week', y, h: WEEK_H, start: weeks[i], end: addWeeks(weeks[i], 1), n: 1 }
      segs.push(s)
      weekSeg.set(weeks[i], s)
      y += WEEK_H
    }
    i = j
  }
  const timeH = y
  const yOf = (ms: number): number => {
    const s = weekSeg.get(monday(Math.max(ms - 1, 0)))
    if (!s) return timeH
    return Math.min(s.y + WEEK_H, Math.max(s.y, s.y + ((s.end - ms) / DAY) * PPD))
  }
  const yt: Record<string, number> = {}
  const yb: Record<string, number> = {}
  for (const it of shown) {
    const top = yOf(Math.min(it.last + DAY, addWeeks(monday(it.last), 1)))
    yt[it.key] = top
    yb[it.key] = Math.max(yOf(Math.max(it.first || it.last, oldestWeek + 1)), top + 6)
  }
  return { segs, shown, olderCount, cutoff: capStart, timeH, yt, yb }
}

// ── citation chips (idea copied from LiveGlobe.tsx SourcedText) ─────────────
const PAGE_CSS = `
.sp{background:${C.bg};color:${C.text};font-family:${MONO};font-size:13px;line-height:1.45;min-height:100vh}
.sp *{box-sizing:border-box}
.sp a{color:inherit;text-decoration:none}
.sp button{font:inherit;color:inherit;background:none;border:1px solid ${C.border};border-radius:4px;padding:2px 8px;cursor:pointer}
.sp button:hover{border-color:${C.border2};background:${C.panel2}}
.sp button.on{border-color:${C.green};color:${C.green}}
.sp-top{display:flex;align-items:center;gap:14px;padding:10px 16px;border-bottom:1px solid ${C.border};background:${C.panel};position:sticky;top:0;z-index:20}
.sp-brand{color:${C.green};font-weight:600;letter-spacing:.04em;white-space:nowrap}
.sp-top form{flex:1;min-width:160px}
.sp-top input{font:inherit;color:${C.text};background:${C.bg};border:1px solid ${C.border};border-radius:6px;padding:9px 12px;width:100%;outline:none}
.sp-top input:focus{border-color:${C.green}}
.sp-wrap{max-width:1440px;margin:0 auto;padding:16px;display:grid;grid-template-columns:minmax(0,1fr) 360px;gap:20px;align-items:start}
.sp-main{display:flex;flex-direction:column;gap:16px;min-width:0}
.sp-card{background:${C.panel};border:1px solid ${C.border};border-radius:8px;padding:16px 18px}
.sp-kick{font-size:11px;color:${C.dim};letter-spacing:.08em;text-transform:uppercase;display:flex;gap:10px;flex-wrap:wrap}
.sp h1{font-size:20px;margin:6px 0 10px;font-weight:600;line-height:1.3}
.sp-prose{font-family:system-ui,-apple-system,sans-serif;font-size:15px;line-height:1.65}
.sp-prose p{margin:0 0 10px}
.sp-hi{margin:8px 0 0;padding:0;list-style:none;display:grid;gap:6px}
.sp-hi li{font-family:system-ui,-apple-system,sans-serif;font-size:13.5px;padding-left:14px;position:relative}
.sp-hi li::before{content:'›';position:absolute;left:0;color:${C.green}}
.sp-cite{display:inline-flex;align-items:center;font-family:${MONO};font-size:10.5px;padding:0 5px;margin:0 2px;border-radius:3px;border:1px solid var(--c);color:var(--c);cursor:pointer;vertical-align:2px;white-space:nowrap}
.sp-cite.hl{background:var(--c);color:${C.bg}}
.sp-dim{color:${C.dim}}.sp-dim2{color:${C.dim2}}
.sp-note{font-size:12px;color:${C.dim}}
.sp-banner{border:1px solid ${C.green};border-radius:8px;padding:10px 14px;background:${C.panel};font-size:12px;display:flex;gap:12px;align-items:center;flex-wrap:wrap}
.sp-banner b{color:${C.green}}
.sp-banner a{margin-left:auto;border:1px solid ${C.green};color:${C.green};border-radius:4px;padding:2px 8px}
.sp-threads{padding:12px 14px}
.sp-tr{display:grid;grid-template-columns:14px minmax(0,1fr) auto;gap:8px;align-items:start;padding:7px 8px;border-radius:6px;border:1px solid transparent;cursor:pointer}
.sp-tr:hover,.sp-tr.hl{border-color:${C.green};background:${C.panel2}}
.sp-tr .g{font-size:11px;padding-top:2px}
.sp-tr .ti{font-size:12.5px;font-weight:500;line-height:1.3;overflow-wrap:anywhere}
.sp-tr .m{font-size:10.5px;color:${C.dim};display:flex;gap:8px;flex-wrap:wrap;margin-top:2px}
.sp-tr .lr{font-size:10.5px;color:${C.dim2};text-align:right;white-space:nowrap}
.sp-tr .lr.fresh{color:${C.green}}
.sp-tr .lr b{display:block;font-weight:500;color:inherit}
.sp-ledger{padding:0}
.sp-lhead{position:sticky;top:53px;z-index:12;background:${C.panel};border-bottom:1px solid ${C.border};border-radius:8px 8px 0 0;padding:8px 14px;display:flex;gap:10px;align-items:center;flex-wrap:wrap;font-size:11px;color:${C.dim}}
.sp-lhead b{color:${C.text};font-weight:600;font-size:12px}
.sp-lhead .f{display:flex;flex-wrap:wrap;gap:4px;margin-left:auto}
.sp-lhead .f button{font-size:10px;padding:1px 6px}
.sp-body{display:grid;grid-template-columns:64px 84px minmax(0,1fr);position:relative}
.sp-gut{position:relative;border-right:1px solid ${C.border}}
.sp-seg{border-top:1px solid ${C.grid};position:relative}
.sp-seg .lbl{position:sticky;top:94px;padding:6px 8px;font-size:11px;color:${C.dim};line-height:1.3}
.sp-seg .lbl b{color:${C.text};font-weight:600;display:block}
.sp-seg .lbl .fresh{color:${C.green}}
.sp-seg.quiet{display:flex;align-items:center;padding:0 8px;font-size:10px;color:${C.dim2};background:${C.panel2}}
.sp-band{position:relative;border-right:1px solid ${C.border}}
.sp-sbar{position:absolute;width:8px;border-radius:4px;background:color-mix(in srgb,var(--c) 35%,${C.panel});border:1px solid var(--c);cursor:pointer}
.sp-sbar.dashed{border-style:dashed}.sp-sbar.dotted{border-style:dotted;background:transparent}.sp-sbar.faint{opacity:.55;background:transparent}
.sp-sbar.fresh::before{content:'';position:absolute;left:-3px;right:-3px;top:-3px;height:3px;border-radius:2px;background:${C.green}}
.sp-sbar.hl,.sp-sbar.sel{box-shadow:0 0 0 2px ${C.green};z-index:2}
.sp-list{position:relative}
.sp-leads{position:absolute;top:0;pointer-events:none;overflow:visible}
.sp-leads path{fill:none;stroke:${C.border2};stroke-width:1}
.sp-leads path.hl{stroke:${C.green};stroke-width:1.5}
.sp-row{position:absolute;left:10px;right:12px;border:1px solid ${C.border};border-left:3px solid var(--c);border-radius:6px;background:${C.panel};padding:7px 10px;cursor:pointer;display:grid;gap:3px;overflow:hidden}
.sp-row.dashed{border-left-style:dashed}.sp-row.dotted{border-left-style:dotted}.sp-row.faint{opacity:.78}
.sp-row:hover,.sp-row.hl{border-color:${C.green}}
.sp-row.sel{border-color:${C.green};box-shadow:0 0 0 1px ${C.green};background:${C.panel2};overflow:visible;z-index:3}
.sp-row .h{display:flex;gap:8px;font-size:10.5px;color:${C.dim};flex-wrap:wrap;min-width:0}
.sp-row .h .ag{margin-left:auto}.sp-row .h .ag.fresh{color:${C.green}}
.sp-row .ti{font-size:12.5px;font-weight:500;line-height:1.35;overflow-wrap:anywhere}
.sp-row .ti .k{color:${C.blue}}
.sp-row .ov{font-family:system-ui,-apple-system,sans-serif;font-size:12px;color:${C.dim};line-height:1.45;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.sp-row.sel .ov{-webkit-line-clamp:unset;color:${C.text};font-size:13px}
.sp-pp{font-size:10.5px;color:${C.dim}}
.sp-why{border-left:3px solid var(--evc);padding:6px 10px;background:${C.panel};border-radius:0 4px 4px 0;font-size:12px;margin-top:4px}
.sp-open{display:inline-block;margin-top:6px;border:1px solid ${C.border};border-radius:4px;padding:2px 8px;width:max-content}
.sp-prs{font:inherit;font-size:10.5px;color:${C.blue};background:transparent;border:1px solid ${C.border};border-radius:4px;padding:0 6px;cursor:pointer}
.sp-prs:hover{border-color:${C.green}}
.sp-prs:focus-visible{outline:2px solid ${C.green};outline-offset:1px}
.sp-prlist{list-style:none;margin:4px 0 0;padding:0;font-size:12px;display:grid;gap:3px;min-width:0}
.sp-prlist li{overflow-wrap:anywhere;min-width:0}
.sp-prlist a{color:${C.blue};white-space:nowrap}
.sp-rail{position:sticky;top:64px;display:flex;flex-direction:column;gap:12px;max-height:calc(100vh - 80px);overflow:auto}
.sp-rail .sp-card{padding:12px 14px}
.sp-rt{font-size:11px;color:${C.dim};letter-spacing:.06em;text-transform:uppercase;margin-bottom:8px}
.sp-grp{margin-bottom:10px}
.sp-gh{display:flex;align-items:center;gap:8px;font-weight:600;font-size:12px;padding:4px 0}
.sp-gh i{font-style:normal;width:14px;text-align:center}
.sp-gh .n{color:${C.dim};font-weight:400}
.sp-gh .lat{margin-left:auto;color:${C.dim2};font-weight:400;font-size:11px}
.sp-gh .lat.fresh{color:${C.green}}
.sp-rr{display:grid;grid-template-columns:12px 1fr auto;gap:6px;align-items:center;padding:3px 4px;border-radius:3px;font-size:11.5px;cursor:pointer}
.sp-rr:hover,.sp-rr.hl{background:${C.panel2}}
.sp-rr.sel{outline:1px solid ${C.green}}
.sp-rr .t{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.sp-rr .a{color:${C.dim2};font-size:10.5px}
.sp-ppl{display:flex;flex-wrap:wrap;gap:4px}
.sp-ppl span{font-size:11px;padding:2px 8px;border:1px solid ${C.border};border-radius:999px}
.sp-ppl span i{font-style:normal;color:${C.dim}}
.sp-older{padding:10px 14px;border-top:1px solid ${C.border};font-size:12px;color:${C.dim}}
.sp-older button{margin-left:8px}
@media (max-width:900px){
.sp-wrap{grid-template-columns:1fr;gap:14px}
.sp-rail{position:static;max-height:none}
.sp-prose{font-size:14px}.sp h1{font-size:17px}.sp-card{padding:12px 14px}
.sp-body{grid-template-columns:44px 52px minmax(0,1fr)}
.sp-seg .lbl{padding:4px;font-size:10px}
.sp-sbar{width:6px}
.sp-row{left:6px;right:6px;padding:6px 8px}
.sp-tr .lr{max-width:90px;white-space:normal}
}
`

function Chips({
  text,
  sources,
  keyOf,
  hl,
  setHl,
  pick,
}: {
  text: string
  sources?: ClusterSummary['sources']
  keyOf: (nodeId: string) => string
  hl: string | null
  setHl: (k: string | null) => void
  pick: (k: string) => void
}) {
  if (!sources) return <>{text.replace(/\s*\[[TR]\d+\]/g, '')}</>
  const parts = text.split(/(\[[TR]\d+\])/g)
  return (
    <>
      {parts.map((p, i) => {
        const m = /^\[([TR]\d+)\]$/.exec(p)
        if (!m) return <span key={i}>{p}</span>
        const src = sources[m[1]]
        if (!src) return null
        const k = keyOf(src.node_id)
        const color = src.node_id.startsWith('slack:')
          ? C.green
          : src.node_id.startsWith('jira:')
            ? C.blue
            : src.node_id.startsWith('gh_pr:')
              ? C.purple
              : C.amber
        return (
          <a
            key={i}
            className={`sp-cite${hl === k ? ' hl' : ''}`}
            style={{ ['--c' as string]: color }}
            data-sync={k}
            title={src.label}
            onMouseEnter={() => setHl(k)}
            onMouseLeave={() => setHl(null)}
            onClick={() => pick(k)}
          >
            {src.label.split(' — ')[0]}
          </a>
        )
      })}
    </>
  )
}

function People({ it, expanded }: { it: Item; expanded: boolean }) {
  const names = it.participants.slice(0, 3)
  const more = Math.max(0, it.participantCount - names.length)
  if (names.length === 0) return null
  return (
    <div className="sp-pp">
      {names.join(', ')}
      {more > 0 ? (expanded ? ` + ${more} more` : ` +${more}`) : ''}
    </div>
  )
}

function PRList({ count, prs }: { count: number; prs: PRRef[] }) {
  return (
    <ul className="sp-prlist" onClick={(e) => e.stopPropagation()}>
      {prs.map((p) => (
        <li key={p.node_id}>
          <span>{p.title || p.node_id.replace(/^gh_pr:/, '')}</span>
          {p.author && <span> · {p.author}</span>}
          {p.created_ms > 0 && <span> · {fmtDay(p.created_ms)}</span>}{' '}
          {p.url ? (
            <a href={p.url} target="_blank" rel="noopener noreferrer" onClick={(e) => e.stopPropagation()}>
              open ↗
            </a>
          ) : (
            <span style={{ color: C.dim2 }}>no link</span>
          )}
        </li>
      ))}
      {count > prs.length && <li>+{count - prs.length} more</li>}
    </ul>
  )
}

// ── page ─────────────────────────────────────────────────────────────────────

function initialQuery(): string {
  return new URLSearchParams(window.location.search).get('q')?.trim() || ''
}

export function GraphSearchPage() {
  const [input, setInput] = useState(initialQuery)
  const [status, setStatus] = useState<Status>(() => (initialQuery() ? { kind: 'loading' } : { kind: 'idle' }))
  const [filter, setFilter] = useState<Ev | ''>('')
  const [sel, setSel] = useState<string | null>(null)
  const [hl, setHl] = useState<string | null>(null)
  const [allThreads, setAllThreads] = useState(false)
  const [showOlder, setShowOlder] = useState(false)
  const [heights, setHeights] = useState<Record<string, number>>({})
  const [openPRs, setOpenPRs] = useState<Set<string>>(() => new Set())
  const togglePRs = (key: string) =>
    setOpenPRs((cur) => {
      const next = new Set(cur)
      if (!next.delete(key)) next.add(key)
      return next
    })
  const [narrow, setNarrow] = useState(() => window.matchMedia('(max-width: 900px)').matches)
  const gen = useRef(0) // stale-response guard, same pattern as LiveGlobe searchGen
  const listRef = useRef<HTMLDivElement>(null)

  const run = useCallback(async (query: string) => {
    const g = ++gen.current
    const stale = () => g !== gen.current
    const q = query.trim()
    const freeFlow = async (notice: string) => {
      try {
        const res = await graphSearchHybrid(q, 50)
        if (stale()) return
        const items = buildFreeItems(res.results || [])
        if (items.length === 0) {
          setStatus({ kind: 'ready', view: { mode: 'free', q, items, nodeRoot: {}, summary: 'none', notice, semErr: !!res.semantic_error } })
          return
        }
        setStatus({
          kind: 'ready',
          view: {
            mode: 'free',
            q,
            items,
            nodeRoot: {},
            summary: 'none',
            notice,
            semErr: !!res.semantic_error,
            banner: pickBanner(res.results || []),
          },
        })
      } catch (e) {
        if (!stale()) setStatus({ kind: 'error', msg: String((e as Error).message || e) })
      }
    }

    const seed = parseSlackLink(q)?.nodeId ?? parseGraphSeed(q)
    if (!seed) {
      await freeFlow('')
      return
    }
    try {
      const resolved = await graphResolve([seed], undefined, 1)
      if (stale()) return
      const hop0 = (resolved.artifacts || []).find((a) => a.hop === 0)
      if (!hop0) {
        await freeFlow(`not in the graph yet: ${q}`)
        return
      }
      const seedRoot = hop0.node_id.startsWith('slack:') ? slackRootOf(hop0.node_id, hop0.thread_ts) : hop0.node_id
      const summaryP = fetchClusterSummary(hop0.node_id, 2).catch(() => null)
      let nb = await graphNeighborsCards(hop0.node_id, 2)
      let rows = nb.neighbors
      if (stale()) return
      const build = (rs: GraphNeighbor[]): View => {
        const { items, nodeRoot } = buildSeedItems(rs, seedRoot)
        return {
          mode: 'seed',
          q,
          seed: { id: hop0.node_id, type: hop0.type, title: hop0.title || hop0.node_id },
          seedPRs: nb.seed_prs,
          items,
          nodeRoot,
          summary: 'loading',
          notice: '',
          semErr: false,
        }
      }
      let view = build(rows)
      setStatus({ kind: 'ready', view })
      let subjectItems: Item[] = []
      if (hop0.node_id.startsWith('jira:')) {
        setStatus((cur) =>
          !stale() && cur.kind === 'ready' && cur.view.q === q
            ? { kind: 'ready', view: { ...cur.view, notice: 'finding same-subject threads…' } }
            : cur,
        )
        void (async () => {
          try {
            const { queries, error } = await graphSubjectQueries(hop0.node_id)
            if (stale() || error || queries.length === 0) return
            const results = await Promise.all(queries.map((query) => graphSearchHybrid(query, 20, 'slack,slack_thread')))
            if (stale()) return
            const seen = new Set<string>()
            subjectItems = results.flatMap((result, i) =>
              buildFreeItems(result.results || [])
                .filter((item) => {
                  if (item.group !== 'slack' || seen.has(item.key)) return false
                  seen.add(item.key)
                  return true
                })
                .map((item): Item => ({
                  ...item,
                  ev: 'subject',
                  why: `same subject as the ticket: "${queries[i]}"`,
                })),
            )
            setStatus((cur) => {
              if (stale() || cur.kind !== 'ready' || cur.view.q !== q) return cur
              const keys = new Set(cur.view.items.map((item) => item.key))
              const extra = subjectItems.filter((item) => !keys.has(item.key))
              return { kind: 'ready', view: { ...cur.view, items: [...cur.view.items, ...extra] } }
            })
          } catch {
            // Subject search is optional; the linked items remain usable on failure.
          } finally {
            setStatus((cur) =>
              !stale() && cur.kind === 'ready' && cur.view.q === q
                ? { kind: 'ready', view: { ...cur.view, notice: '' } }
                : cur,
            )
          }
        })()
      }
      void summaryP.then((s) => {
        if (stale()) return
        setStatus((cur) =>
          cur.kind === 'ready' && cur.view.q === q ? { kind: 'ready', view: { ...cur.view, summary: s ?? 'none' } } : cur,
        )
      })
      // Summaries for raw threads are enqueued server-side; re-poll like /live does.
      for (let attempt = 0; attempt < 3 && rows.some((r) => r.node.pending_summary); attempt++) {
        await new Promise((r) => setTimeout(r, 12_000))
        if (stale()) return
        nb = await graphNeighborsCards(hop0.node_id, 2)
        rows = nb.neighbors
        if (stale()) return
        view = build(rows)
        setStatus((cur) => {
          if (stale() || cur.kind !== 'ready' || cur.view.q !== q) return cur
          const keys = new Set(view.items.map((item) => item.key))
          const extra = subjectItems.filter((item) => !keys.has(item.key))
          return {
            kind: 'ready',
            view: { ...view, items: [...view.items, ...extra], notice: cur.view.notice, summary: cur.view.summary },
          }
        })
      }
    } catch (e) {
      if (!stale()) setStatus({ kind: 'error', msg: String((e as Error).message || e) })
    }
  }, [])

  useEffect(() => {
    const q = initialQuery()
    if (!q) return
    const t = window.setTimeout(() => void run(q), 0)
    return () => window.clearTimeout(t)
  }, [run])

  useEffect(() => {
    const mq = window.matchMedia('(max-width: 900px)')
    const on = () => setNarrow(mq.matches)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    const q = input.trim()
    const url = new URL(window.location.href)
    if (q) url.searchParams.set('q', q)
    else url.searchParams.delete('q')
    window.history.replaceState(null, '', url)
    setFilter('')
    setSel(null)
    setAllThreads(false)
    setShowOlder(false)
    if (!q) {
      gen.current++ // drop any in-flight response
      setStatus({ kind: 'idle' })
      return
    }
    setStatus({ kind: 'loading' })
    void run(q)
  }

  const view = status.kind === 'ready' ? status.view : null
  const items = useMemo(() => {
    if (!view) return []
    return filter ? view.items.filter((i) => i.ev === filter) : view.items
  }, [view, filter])
  const threads = useMemo(() => (view ? threadOrder(items, view.mode) : []), [view, items])
  const story = useMemo(() => layoutStory(items, showOlder), [items, showOlder])

  // Real card heights → non-overlapping placement (the mockup's relayout()).
  useEffect(() => {
    const root = listRef.current
    if (!root) return
    const ro = new ResizeObserver((entries) => {
      setHeights((cur) => {
        const next = { ...cur }
        let changed = false
        for (const en of entries) {
          const el = en.target as HTMLElement
          const k = el.dataset.row
          if (!k) continue
          const h = el.offsetHeight
          if (next[k] !== h) {
            next[k] = h
            changed = true
          }
        }
        return changed ? next : cur
      })
    })
    root.querySelectorAll<HTMLElement>('[data-row]').forEach((el) => ro.observe(el))
    return () => ro.disconnect()
  }, [story])

  const placed = useMemo(() => {
    const sorted = [...story.shown].sort((a, b) => b.last - a.last)
    const top: Record<string, number> = {}
    let cursor = 0
    for (const it of sorted) {
      const t = Math.max(story.yt[it.key] ?? 0, cursor)
      top[it.key] = t
      cursor = t + (heights[it.key] ?? 96) + 6
    }
    return { sorted, top, total: Math.max(story.timeH, cursor) + 10 }
  }, [story, heights])

  const pick = useCallback((key: string) => {
    setSel(key)
    window.setTimeout(() => {
      document.querySelector(`[data-row="${CSS.escape(key)}"]`)?.scrollIntoView({ behavior: 'smooth', block: 'center' })
    }, 60)
  }, [])

  const keyOf = useCallback((nodeId: string) => view?.nodeRoot[nodeId] ?? nodeId, [view])

  const hlProps = (k: string) => ({
    'data-sync': k,
    onMouseEnter: () => setHl(k),
    onMouseLeave: () => setHl(null),
  })

  const people = useMemo(() => {
    const counts = new Map<string, number>()
    for (const it of items) {
      if (it.group !== 'slack') continue
      for (const n of new Set([it.rootAuthor, ...it.participants].filter(Boolean))) counts.set(n, (counts.get(n) || 0) + 1)
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, 10)
  }, [items])

  const bandW = narrow ? 52 : 84
  const gutW = narrow ? 44 : 64
  const slotW = (bandW - 8) / GROUPS.length
  const xOf = (g: Group) => 4 + GROUP_META[g].slot * slotW + slotW / 2
  const evClass = (ev: Ev) => (ev === 'judge' || ev === 'semantic' ? ' dashed' : ev === 'similar' ? ' dotted' : ev === 'hop2' ? ' faint' : '')

  const filters = view?.mode === 'free' ? FREE_FILTERS : view?.seed?.id.startsWith('jira:') ? [...SEED_FILTERS, 'subject' as Ev] : SEED_FILTERS
  const shownThreads = allThreads ? threads : threads.slice(0, 6)

  function briefing(v: View) {
    const s = v.summary
    return (
      <div className="sp-card">
        <div className="sp-kick">
          <span>{v.seed?.type}</span>
          <span>{v.items.length} related items</span>
        </div>
        <h1>
          {v.seed?.title}
          {v.seedPRs && v.seedPRs.pr_count > 0 && (
            <>
              {' '}
              <button className="sp-prs" onClick={() => togglePRs('seed')}>
                ⎇ {v.seedPRs.pr_count}
              </button>
            </>
          )}
        </h1>
        {v.seedPRs && v.seedPRs.pr_count > 0 && openPRs.has('seed') && <PRList count={v.seedPRs.pr_count} prs={v.seedPRs.prs} />}
        {s === 'loading' ? (
          <div className="sp-note">summarizing…</div>
        ) : s === 'none' ? null : (
          <div className="sp-prose">
            <p>
              <Chips text={s.overview || ''} sources={s.sources} keyOf={keyOf} hl={hl} setHl={setHl} pick={pick} />
            </p>
            {s.highlights?.length ? (
              <ul className="sp-hi">
                {s.highlights.map((h, i) => (
                  <li key={i}>
                    <Chips text={h} sources={s.sources} keyOf={keyOf} hl={hl} setHl={setHl} pick={pick} />
                  </li>
                ))}
              </ul>
            ) : null}
          </div>
        )}
      </div>
    )
  }

  function threadRow(it: Item) {
    const em = EV_META[it.ev]
    return (
      <div key={it.key} className={`sp-tr${hl === it.key ? ' hl' : ''}`} {...hlProps(it.key)} onClick={() => pick(it.key)}>
        <span className="g" style={{ color: em.color }}>
          {em.glyph}
        </span>
        <div>
          <div className="ti">{it.title}</div>
          <div className="m">
            {it.rootAuthor && <span>started by {it.rootAuthor}</span>}
            {it.channel && <span>#{it.channel}</span>}
            {it.first > 0 && <span>{fmtDT(it.first)}</span>}
            {it.msgCount > 0 && <span>{it.msgCount} msgs</span>}
            {it.participants.length > 0 && (
              <span>
                {it.participants.slice(0, 3).join(', ')}
                {it.participantCount > 3 ? ` +${it.participantCount - 3}` : ''}
              </span>
            )}
            <span style={{ color: em.color }}>{em.label}</span>
          </div>
        </div>
        <div className={`lr${it.last > 0 && isFresh(it.last) ? ' fresh' : ''}`}>{it.last > 0 ? `last reply ${ago(it.last)}` : ''}</div>
      </div>
    )
  }

  function storyCard(it: Item) {
    const gm = GROUP_META[it.group]
    const em = EV_META[it.ev]
    const open = sel === it.key
    return (
      <div
        key={it.key}
        className={`sp-row${evClass(it.ev)}${open ? ' sel' : ''}${hl === it.key ? ' hl' : ''}`}
        data-row={it.key}
        {...hlProps(it.key)}
        style={{ ['--c' as string]: gm.color, ['--evc' as string]: em.color, top: placed.top[it.key] ?? 0 }}
        onClick={() => setSel(open ? null : it.key)}
      >
        <div className="h">
          <span style={{ color: em.color }}>
            {em.glyph} {em.label}
          </span>
          {it.group === 'slack' ? (
            <>
              {it.channel && <span>#{it.channel}</span>}
              {it.rootAuthor && <span>started by {it.rootAuthor}</span>}
              {it.first > 0 && <span>{fmtDT(it.first)}</span>}
              {it.msgCount > 0 && <span>{it.msgCount} msgs</span>}
            </>
          ) : (
            <span>
              {gm.icon} {gm.label}
            </span>
          )}
          {it.last > 0 && (
            <span className={`ag${isFresh(it.last) ? ' fresh' : ''}`}>
              {it.group === 'slack' ? `last reply ${ago(it.last)}` : `${fmtDay(it.last)} · ${ago(it.last)}`}
            </span>
          )}
          {it.group === 'jira' && it.prCount > 0 && (
            <button
              className="sp-prs"
              onClick={(e) => {
                e.stopPropagation()
                togglePRs(it.key)
              }}
            >
              ⎇ {it.prCount}
            </button>
          )}
        </div>
        <div className="ti">
          {it.jiraKey && <span className="k">{it.jiraKey} </span>}
          {it.title}
        </div>
        {it.group === 'jira' && it.prCount > 0 && openPRs.has(it.key) && <PRList count={it.prCount} prs={it.prs} />}
        {it.overview && <div className="ov">{it.overview}</div>}
        {it.group === 'slack' && <People it={it} expanded={open} />}
        {open && (
          <>
            <div className="sp-why">
              <b style={{ color: em.color }}>why is this here · </b>
              {it.why}
            </div>
            {it.url && (
              <a className="sp-open" href={it.url} target="_blank" rel="noopener noreferrer" onClick={(e) => e.stopPropagation()}>
                open ↗
              </a>
            )}
          </>
        )}
      </div>
    )
  }

  function storySection() {
    return (
      <div className="sp-card sp-ledger">
        <div className="sp-lhead">
          <b>Story</b>
          <span>newest at the top</span>
          <span className="f">
            <button className={filter === '' ? 'on' : ''} onClick={() => setFilter('')}>
              all
            </button>
            {filters.map((f) => (
              <button key={f} className={filter === f ? 'on' : ''} onClick={() => setFilter(f)}>
                {EV_META[f].glyph} {EV_META[f].label}
              </button>
            ))}
          </span>
        </div>
        <div className="sp-body">
          <div className="sp-gut" style={{ height: placed.total }}>
            {story.segs.map((s, i) =>
              s.kind === 'quiet' ? (
                <div key={i} className="sp-seg quiet" style={{ height: s.h }}>
                  {s.n} quiet weeks
                </div>
              ) : (
                <div key={i} className="sp-seg" style={{ height: s.h }}>
                  <div className="lbl">
                    <b>{fmtDay(s.start)}</b>– {fmtDay(s.end - DAY)}
                    {Date.now() >= s.start && Date.now() < s.end && <div className="fresh">this week</div>}
                  </div>
                </div>
              ),
            )}
          </div>
          <div className="sp-band" style={{ height: placed.total }}>
            {story.segs.map((s) => (
              <div key={s.y} className="sp-seg" style={{ position: 'absolute', left: 0, right: 0, top: s.y, height: s.h }} />
            ))}
            {placed.sorted.map((it) => {
              const em = EV_META[it.ev]
              return (
                <div
                  key={it.key}
                  className={`sp-sbar${evClass(it.ev)}${isFresh(it.last) ? ' fresh' : ''}${sel === it.key ? ' sel' : ''}${hl === it.key ? ' hl' : ''}`}
                  {...hlProps(it.key)}
                  title={`${em.label}: ${it.title}`}
                  style={{
                    ['--c' as string]: GROUP_META[it.group].color,
                    left: xOf(it.group) - 4,
                    top: story.yt[it.key],
                    height: Math.max(6, story.yb[it.key] - story.yt[it.key]),
                  }}
                  onClick={() => pick(it.key)}
                />
              )
            })}
          </div>
          <div className="sp-list" ref={listRef} style={{ height: placed.total }}>
            {placed.sorted.map((it) => storyCard(it))}
          </div>
          <svg className="sp-leads" width={bandW + 12} height={placed.total} style={{ left: gutW, width: bandW + 12, height: placed.total }}>
            {placed.sorted.map((it) => {
              const yt = story.yt[it.key] + 1
              const yr = (placed.top[it.key] ?? 0) + 14
              const x0 = xOf(it.group)
              return (
                <path
                  key={it.key}
                  className={hl === it.key || sel === it.key ? 'hl' : ''}
                  d={`M${x0},${yt} L${bandW},${yt} C${bandW + 6},${yt} ${bandW + 2},${yr} ${bandW + 10},${yr}`}
                />
              )
            })}
          </svg>
        </div>
        {story.olderCount > 0 && !showOlder && (
          <div className="sp-older">
            {story.olderCount} older items (before {fmtDay(story.cutoff)}) —{" "}
            <button onClick={() => setShowOlder(true)}>show</button>
          </div>
        )}
      </div>
    )
  }

  function rail() {
    return (
      <div className="sp-rail">
        <div className="sp-card">
          <div className="sp-rt">By resource</div>
          {GROUPS.map((g) => {
            const rows = items.filter((i) => i.group === g.key).sort((a, b) => b.last - a.last)
            if (rows.length === 0) return null
            const latest = rows[0].last
            return (
              <div className="sp-grp" key={g.key}>
                <div className="sp-gh">
                  <i style={{ color: g.color }}>{g.icon}</i>
                  {g.label} <span className="n">{rows.length}</span>
                  {latest > 0 && <span className={`lat${isFresh(latest) ? ' fresh' : ''}`}>latest {ago(latest)}</span>}
                </div>
                {rows.map((it) => (
                  <div
                    key={it.key}
                    className={`sp-rr${hl === it.key ? ' hl' : ''}${sel === it.key ? ' sel' : ''}`}
                    {...hlProps(it.key)}
                    title={EV_META[it.ev].label}
                    onClick={() => pick(it.key)}
                  >
                    <span style={{ color: EV_META[it.ev].color, textAlign: 'center' }}>{EV_META[it.ev].glyph}</span>
                    <span className="t">
                      {it.jiraKey ? `${it.jiraKey} ` : ''}
                      {it.group === 'slack' && it.channel ? `#${it.channel} · ` : ''}
                      {it.title}
                    </span>
                    <span className="a">
                      {it.group === 'jira' && it.prCount > 0 ? `⎇ ${it.prCount}${it.last > 0 ? ` · ${ago(it.last)}` : ''}` : it.last > 0 ? ago(it.last) : ''}
                    </span>
                  </div>
                ))}
              </div>
            )
          })}
          {people.length > 0 && (
            <div className="sp-grp">
              <div className="sp-gh">
                <i>◉</i>People
              </div>
              <div className="sp-ppl">
                {people.map(([n, c]) => (
                  <span key={n}>
                    {n} <i>{c}</i>
                  </span>
                ))}
              </div>
            </div>
          )}
        </div>
      </div>
    )
  }

  return (
    <div className="sp">
      <style>{PAGE_CSS}</style>
      <div className="sp-top">
        <a className="sp-brand" href="/live">
          agent-mem
        </a>
        <form onSubmit={submit}>
          <input
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="Jira key, Slack link, URL, or any text"
            autoFocus
          />
        </form>
      </div>
      <div className="sp-wrap">
        <div className="sp-main">
          {status.kind === 'idle' && <div className="sp-note">type a Jira key, a Slack link, any URL, or some words</div>}
          {status.kind === 'loading' && <div className="sp-note">loading…</div>}
          {status.kind === 'error' && <div className="sp-note" style={{ color: C.red }}>error: {status.msg}</div>}
          {view && (
            <>
              {view.notice && <div className="sp-note">{view.notice}</div>}
              {view.semErr && <div className="sp-note">semantic search unavailable, showing keyword matches</div>}
              {view.mode === 'seed' ? (
                briefing(view)
              ) : (
                <div className="sp-card">
                  <div className="sp-kick">
                    <span>free text</span>
                    <span>{view.items.length} hits</span>
                  </div>
                  <h1>“{view.q}”</h1>
                </div>
              )}
              {view.mode === 'free' && view.banner && (
                <div className="sp-banner">
                  <span>
                    <b>briefing available</b> — {view.banner.label}
                  </span>
                  <a href={`/search?q=${encodeURIComponent(view.banner.value)}`}>open the briefing for {view.banner.label} →</a>
                </div>
              )}
              {view.items.length === 0 ? (
                <div className="sp-note">no matches for {view.q}</div>
              ) : (
                <>
                  {threads.length > 0 && (
                    <div className="sp-card sp-threads">
                      <div className="sp-kick" style={{ marginBottom: 8 }}>
                        <span>Slack threads · {threads.length}</span>
                      </div>
                      {shownThreads.map(threadRow)}
                      {threads.length > 6 && (
                        <div className="sp-note" style={{ marginTop: 4 }}>
                          <button onClick={() => setAllThreads(!allThreads)}>
                            {allThreads ? 'show fewer' : `+${threads.length - 6} more`}
                          </button>
                        </div>
                      )}
                    </div>
                  )}
                  {storySection()}
                </>
              )}
            </>
          )}
        </div>
        {view && view.items.length > 0 && rail()}
      </div>
    </div>
  )
}
