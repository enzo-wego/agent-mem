import { useMemo, useRef } from 'react'
import ForceGraph2D from 'react-force-graph-2d'
import type { ClusterGraphNode, ClusterGraphEdge } from '../api'

// Per-type node color (matches the overlay's source grouping).
const TYPE_COLOR: Record<string, string> = {
  jira: '#ffaa00',
  gh_pr: '#b48cff',
  cf: '#4aa3ff',
  cf_page: '#4aa3ff',
  gws_doc: '#4aa3ff',
  gws: '#4aa3ff',
  slack: '#44ff88',
  slack_thread: '#44ff88',
  slack_file: '#7fd1b9',
  feature: '#ff5599',
  person: '#999999',
}

// Types worth labelling on the canvas (anchors). Slack messages stay as dots to
// avoid a 50-label hairball; their text shows on hover. Files/attachments are
// labelled and sized like a feature anchor so they are a real click target, not
// a 3px dot lost in the hairball.
const LABELLED = new Set(['jira', 'gh_pr', 'cf', 'cf_page', 'gws_doc', 'gws', 'feature', 'slack_file', 'jira_attachment'])

// Friendly legend label per type (groups variants under one entry).
const LEGEND: { color: string; label: string; match: string[] }[] = [
  { color: '#ff5599', label: 'Feature (hub)', match: ['feature'] },
  { color: '#ffaa00', label: 'Jira', match: ['jira'] },
  { color: '#b48cff', label: 'Pull request', match: ['gh_pr'] },
  { color: '#4aa3ff', label: 'Doc (Confluence / Google)', match: ['cf', 'cf_page', 'gws_doc', 'gws'] },
  { color: '#44ff88', label: 'Slack message', match: ['slack', 'slack_thread'] },
  { color: '#7fd1b9', label: 'File', match: ['slack_file'] },
  { color: '#999999', label: 'Person', match: ['person'] },
]

interface GNode {
  id: string
  type: string
  name: string
  url: string
  root: boolean
}

export default function ClusterGraph({
  nodes,
  edges,
  width,
  height,
  onDrill,
}: {
  nodes: ClusterGraphNode[]
  edges: ClusterGraphEdge[]
  width: number
  height: number
  onDrill: (id: string, label: string) => void
}) {
  const fgRef = useRef<any>(null)
  const data = useMemo(() => {
    const ids = new Set(nodes.map((n) => n.id))
    return {
      nodes: nodes.map<GNode>((n) => ({
        id: n.id,
        type: n.type,
        name: n.title || n.id,
        url: n.url,
        root: !!n.root,
      })),
      links: (edges ?? [])
        .filter((e) => ids.has(e.from) && ids.has(e.to))
        .map((e) => ({ source: e.from, target: e.to, kind: e.kind })),
    }
  }, [nodes, edges])

  const present = new Set(nodes.map((n) => n.type))
  const legend = LEGEND.filter((l) => l.match.some((m) => present.has(m)))

  return (
    <div style={{ width, display: 'flex', flexDirection: 'column', gap: 8 }}>
    <div style={{ width, height, borderRadius: 4, overflow: 'hidden', background: '#0a0a0a' }}>
      <ForceGraph2D
        ref={fgRef}
        graphData={data}
        width={width}
        height={height}
        backgroundColor="#0a0a0a"
        cooldownTicks={80}
        nodeRelSize={4}
        linkColor={() => 'rgba(255,255,255,0.12)'}
        linkWidth={0.6}
        nodeLabel={(n: any) => `${n.name}`}
        onNodeClick={(n: any) => {
          if (n.url) window.open(n.url, '_blank', 'noopener')
          else onDrill(n.id, n.name)
        }}
        nodePointerAreaPaint={(n: any, color: string, ctx: any) => {
          const r = n.root ? 7 : n.type === 'feature' || n.type === 'slack_file' || n.type === 'jira_attachment' ? 6 : 4
          ctx.fillStyle = color
          ctx.beginPath()
          ctx.arc(n.x, n.y, r + 2, 0, 2 * Math.PI)
          ctx.fill()
        }}
        nodeCanvasObject={(n: any, ctx: any, globalScale: number) => {
          const color = TYPE_COLOR[n.type] ?? '#888888'
          const r = n.root ? 6 : n.type === 'feature' || n.type === 'slack_file' || n.type === 'jira_attachment' ? 5 : 3
          ctx.beginPath()
          ctx.arc(n.x, n.y, r, 0, 2 * Math.PI)
          ctx.fillStyle = color
          ctx.fill()
          if (n.root) {
            ctx.lineWidth = 1.5 / globalScale
            ctx.strokeStyle = '#ffffff'
            ctx.stroke()
          }
          // Label anchors (and the root) only, scaled to stay readable.
          if (n.root || LABELLED.has(n.type)) {
            const label = n.name.length > 28 ? n.name.slice(0, 27) + '…' : n.name
            const fontSize = Math.max(2.5, 4 / globalScale)
            ctx.font = `${fontSize}px ui-monospace, monospace`
            ctx.fillStyle = '#cccccc'
            ctx.textAlign = 'left'
            ctx.textBaseline = 'middle'
            ctx.fillText(label, n.x + r + 1.5, n.y)
          }
        }}
      />
    </div>
      <div
        style={{
          display: 'flex',
          flexWrap: 'wrap',
          gap: '4px 14px',
          padding: '0 2px',
          fontFamily: 'ui-monospace, monospace',
        }}
      >
        {legend.map((l) => (
          <span key={l.label} style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
            <span
              style={{
                width: 8,
                height: 8,
                borderRadius: '50%',
                background: l.color,
                flexShrink: 0,
              }}
            />
            <span style={{ color: '#999999', fontSize: 10 }}>{l.label}</span>
          </span>
        ))}
        <span style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
          <span
            style={{
              width: 8,
              height: 8,
              borderRadius: '50%',
              background: 'transparent',
              border: '1.5px solid #ffffff',
              flexShrink: 0,
            }}
          />
          <span style={{ color: '#999999', fontSize: 10 }}>This thread (root)</span>
        </span>
      </div>
    </div>
  )
}
