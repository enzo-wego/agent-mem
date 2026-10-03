import { useState } from 'react'
import type { ThreadDecision } from './api'

interface Props {
  decisions?: ThreadDecision[]
  openQuestions?: string[]
  text: string
  dim: string
  accent: string
  fontSize?: number
}

export default function ThreadDecisions({ decisions = [], openQuestions = [], text, dim, accent, fontSize = 11 }: Props) {
  const [showAll, setShowAll] = useState(false)
  if (decisions.length === 0 && openQuestions.length === 0) return null

  const earlierCount = Math.max(0, decisions.length - 5)
  const shown = showAll ? decisions : decisions.slice(-5)
  const labelStyle = { color: accent, fontSize: 8, letterSpacing: '0.12em', textTransform: 'uppercase' as const }
  const listStyle = { margin: 0, paddingLeft: 16, display: 'flex', flexDirection: 'column' as const, gap: 3 }

  return (
    <div
      data-testid="thread-decisions-block"
      onClick={(e) => e.stopPropagation()}
      style={{ color: text, fontSize, lineHeight: 1.5, display: 'flex', flexDirection: 'column', gap: 6, overflowWrap: 'anywhere' }}
    >
      {decisions.length > 0 && (
        <>
          <div style={labelStyle}>Decisions</div>
          {!showAll && earlierCount > 0 && (
            <button
              type="button"
              data-testid="thread-decisions-more"
              onClick={() => setShowAll(true)}
              style={{ color: dim, background: 'transparent', border: 'none', padding: 0, font: 'inherit', cursor: 'pointer', alignSelf: 'flex-start' }}
            >
              +{earlierCount} earlier
            </button>
          )}
          <ul data-testid="thread-decisions" style={listStyle}>
            {shown.map((decision, i) => (
              <li key={`${decision.ts}:${i}`}>
                {decision.url ? (
                  <a href={decision.url} target="_blank" rel="noopener noreferrer" style={{ color: text, textDecoration: 'underline' }}>
                    {decision.text}
                  </a>
                ) : decision.text}
                <span style={{ color: dim }}>{decision.by && ` · ${decision.by}`} · {decision.date}</span>
              </li>
            ))}
          </ul>
        </>
      )}
      {openQuestions.length > 0 && (
        <>
          <div style={labelStyle}>Open questions</div>
          <ul data-testid="thread-open-questions" style={listStyle}>
            {openQuestions.map((question, i) => <li key={i}>{question}</li>)}
          </ul>
        </>
      )}
    </div>
  )
}
