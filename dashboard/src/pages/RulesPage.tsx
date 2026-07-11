import { useEffect, useState } from 'react'
import { fetchTopicRules, type TopicRules } from '../api'

// /live/rules — renders topic_rules.json, the single source of truth the LLM
// judge applies at runtime. One view shared by the system, agents, and humans.

const MONO = 'ui-monospace, SFMono-Regular, Menlo, monospace'
const C = {
  bg: '#0b0d10',
  panel: 'rgba(255,255,255,0.03)',
  border: 'rgba(255,255,255,0.10)',
  text: '#d7dce2',
  dim: '#7d8590',
  green: '#3fb950',
  red: '#f85149',
  amber: '#d29922',
}

const TAG_COLORS: Record<string, string> = {
  bug_incident: C.red,
  ops_investigation: C.amber,
  feature_business: C.green,
  release_change: '#58a6ff',
  question_howto: '#bc8cff',
  partner_comm: '#f778ba',
}

export function RulesPage() {
  const [rules, setRules] = useState<TopicRules | null>(null)
  const [err, setErr] = useState('')
  useEffect(() => {
    fetchTopicRules().then(setRules).catch((e) => setErr(String(e)))
  }, [])

  if (err) return <div style={{ background: C.bg, color: C.red, minHeight: '100vh', padding: 40, fontFamily: MONO }}>{err}</div>
  if (!rules) return <div style={{ background: C.bg, color: C.dim, minHeight: '100vh', padding: 40, fontFamily: MONO }}>Loading rules…</div>

  const payments = rules.domains?.payments as
    | { note?: string; partners?: string[]; flows?: string[]; method_families?: string[] }
    | undefined
  const otherDomains = (rules.domains?.other as string[]) || []

  const section = (label: string) => (
    <div style={{ color: C.dim, fontSize: 11, letterSpacing: '0.14em', textTransform: 'uppercase', margin: '34px 0 10px' }}>
      {label}
    </div>
  )

  return (
    <div style={{ background: C.bg, color: C.text, minHeight: '100vh', fontFamily: MONO, fontSize: 13, lineHeight: 1.6 }}>
      <div style={{ maxWidth: 920, margin: '0 auto', padding: '36px 24px 90px' }}>
        <div style={{ display: 'flex', alignItems: 'baseline', gap: 14, flexWrap: 'wrap' }}>
          <a href="/live" style={{ color: C.dim, textDecoration: 'none', fontSize: 12 }}>← LIVE</a>
          <h1 style={{ fontSize: 20, fontWeight: 600, margin: 0 }}>Topic linking rules</h1>
          <span style={{ color: C.dim, fontSize: 11 }}>v{rules.version} · {rules.updated} · applied by the LLM judge at runtime</span>
        </div>
        <p style={{ color: C.dim, maxWidth: '72ch', marginTop: 12 }}>{rules.purpose}</p>

        {section('How a link is made')}
        <ol style={{ margin: 0, paddingLeft: 22, color: C.text }}>
          {rules.how_it_works.map((s, i) => (
            <li key={i} style={{ marginBottom: 6 }}>{s.replace(/^\d+\.\s*/, '')}</li>
          ))}
        </ol>

        {section(`Tags · ${rules.tags.length} — classify first, then apply that tag's criteria`)}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          {rules.tags.map((t) => {
            const color = TAG_COLORS[t.tag] ?? C.text
            return (
              <div key={t.tag} style={{ background: C.panel, border: `1px solid ${C.border}`, borderLeft: `3px solid ${color}`, borderRadius: 6, padding: '14px 18px' }}>
                <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, flexWrap: 'wrap' }}>
                  <span style={{ color, fontWeight: 600, fontSize: 13, letterSpacing: '0.04em' }}>{t.tag}</span>
                  <span style={{ color: C.dim, fontSize: 12 }}>{t.classify_when}</span>
                </div>
                <table style={{ borderCollapse: 'collapse', marginTop: 10, width: '100%', fontSize: 12.5 }}>
                  <tbody>
                    <tr>
                      <td style={{ color: C.green, whiteSpace: 'nowrap', verticalAlign: 'top', paddingRight: 14, paddingBottom: 6 }}>SAME when</td>
                      <td style={{ paddingBottom: 6 }}>{t.same_when}</td>
                    </tr>
                    <tr>
                      <td style={{ color: C.red, whiteSpace: 'nowrap', verticalAlign: 'top', paddingRight: 14, paddingBottom: 6 }}>DIFFERENT when</td>
                      <td style={{ paddingBottom: 6 }}>{t.different_when}</td>
                    </tr>
                    <tr>
                      <td style={{ color: C.dim, whiteSpace: 'nowrap', verticalAlign: 'top', paddingRight: 14 }}>examples</td>
                      <td style={{ color: C.dim }}>
                        <div>✓ {t.example_same}</div>
                        <div>✗ {t.example_different}</div>
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
            )
          })}
        </div>

        {section('Tie-breakers — apply across tags')}
        <ul style={{ margin: 0, paddingLeft: 22 }}>
          {rules.tie_breakers.map((tb, i) => (
            <li key={i} style={{ marginBottom: 6 }}>{tb}</li>
          ))}
        </ul>

        {payments && (
          <>
            {section('Payments domain map (from the payments repo)')}
            {payments.note && <p style={{ color: C.dim, fontSize: 12, marginTop: 0 }}>{payments.note}</p>}
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(260px, 1fr))', gap: 12 }}>
              {([['partners', payments.partners], ['flows', payments.flows], ['method families', payments.method_families]] as const).map(
                ([label, items]) =>
                  items && (
                    <div key={label} style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 6, padding: '12px 16px' }}>
                      <div style={{ color: C.dim, fontSize: 10.5, letterSpacing: '0.1em', textTransform: 'uppercase', marginBottom: 8 }}>{label}</div>
                      <div style={{ fontSize: 12, display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                        {items.map((p) => (
                          <span key={p} style={{ border: `1px solid ${C.border}`, borderRadius: 999, padding: '1px 9px', color: C.text }}>{p}</span>
                        ))}
                      </div>
                    </div>
                  ),
              )}
            </div>
          </>
        )}

        {otherDomains.length > 0 && (
          <>
            {section('Other domains')}
            <div style={{ fontSize: 12, display: 'flex', flexWrap: 'wrap', gap: 6 }}>
              {otherDomains.map((d) => (
                <span key={d} style={{ border: `1px solid ${C.border}`, borderRadius: 999, padding: '1px 9px', color: C.dim }}>{d}</span>
              ))}
            </div>
          </>
        )}

        <div style={{ marginTop: 40, paddingTop: 18, borderTop: `1px solid ${C.border}`, color: C.dim, fontSize: 11 }}>
          Source of truth: <span style={{ color: C.text }}>internal/graph/handlers/topic_rules.json</span> (go:embed) · served at /api/graph/topic-rules ·
          the judge's prompt is generated from this same file, so this page always shows what the system actually applies.
        </div>
      </div>
    </div>
  )
}
