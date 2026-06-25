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
// avoid a 50-label hairball; their text shows on hover.
const LABELLED = new Set(['jira', 'gh_pr', 'cf', 'cf_page', 'gws_doc', 'gws', 'feature'])

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
      links: edges
        .filter((e) => ids.has(e.from) && ids.has(e.to))
        .map((e) => ({ source: e.from, target: e.to, kind: e.kind })),
    }
  }, [nodes, edges])

  return (
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
          const r = n.root ? 7 : n.type === 'feature' ? 6 : 4
          ctx.fillStyle = color
          ctx.beginPath()
          ctx.arc(n.x, n.y, r + 2, 0, 2 * Math.PI)
          ctx.fill()
        }}
        nodeCanvasObject={(n: any, ctx: any, globalScale: number) => {
          const color = TYPE_COLOR[n.type] ?? '#888888'
          const r = n.root ? 6 : n.type === 'feature' ? 5 : 3
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
  )
}
