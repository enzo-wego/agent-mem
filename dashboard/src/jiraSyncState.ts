// Health of the Jira freshness poll, derived from GET /api/graph/jira-updates.
// Pure so it can be unit-tested with node --test (see scripts/jiraSyncState.test.ts).

export interface JiraSyncInput {
  enabled: boolean;
  interval_minutes: number;
  last_ok_at: string | null;
  last_error: string;
}

export type JiraSyncState = 'unavailable' | 'off' | 'never' | 'stale' | 'error' | 'ok';

// Order matters: a stale poll is red even if it also recorded an error.
export function jiraSyncState(cfg: JiraSyncInput | null, now: Date): JiraSyncState {
  if (!cfg) return 'unavailable';
  if (!cfg.enabled) return 'off';
  if (!cfg.last_ok_at) return 'never';
  const ageMs = now.getTime() - new Date(cfg.last_ok_at).getTime();
  if (ageMs > 3 * cfg.interval_minutes * 60_000) return 'stale';
  if (cfg.last_error) return 'error';
  return 'ok';
}
