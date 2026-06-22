const BASE = '';

function authHeaders(): HeadersInit {
  const key = localStorage.getItem('agent_mem_api_key');
  if (!key) return {};
  return { Authorization: `Bearer ${key}` };
}

async function authFetch(url: string, init?: RequestInit): Promise<Response> {
  const headers = { ...authHeaders(), ...(init?.headers || {}) };
  const res = await fetch(url, { ...init, headers });
  if (res.status === 401) {
    // Dispatch event so the app can show a login prompt
    window.dispatchEvent(new CustomEvent('agent-mem-unauthorized'));
  }
  return res;
}

export function setApiKey(key: string) {
  localStorage.setItem('agent_mem_api_key', key);
}

export function getApiKey(): string {
  return localStorage.getItem('agent_mem_api_key') || '';
}

export function clearApiKey() {
  localStorage.removeItem('agent_mem_api_key');
}

export interface SearchResult {
  id: number;
  type: string;
  title: string;
  subtitle?: string;
  narrative?: string;
  project: string;
  created_at: string;
  combined_score?: number;
}

export interface ObservationDetail {
  id: number;
  memory_session_id: string;
  project: string;
  type: string;
  title?: string;
  subtitle?: string;
  narrative?: string;
  text?: string;
  facts: string[];
  concepts: string[];
  files_read: string[];
  files_modified: string[];
  discovery_tokens: number;
  created_at: string;
}

export async function getObservation(id: number): Promise<ObservationDetail> {
  const res = await authFetch(`${BASE}/api/observations/${id}`);
  return res.json();
}

export interface StatsResponse {
  observations: number;
  summaries: number;
  prompts: number;
}

export async function fetchStats(project?: string): Promise<StatsResponse> {
  const params = new URLSearchParams();
  if (project) params.set('project', project);
  const res = await authFetch(`${BASE}/api/stats?${params}`);
  return res.json();
}

export interface SearchResponse {
  results: SearchResult[] | null;
  query: string;
  total: number;
}

export interface HealthResponse {
  status: string;
  postgres: boolean;
  pending_messages: number;
}

export interface SyncClientInfo {
  machine_id: string;
  last_push?: string;
  last_pull?: string;
}

export interface SyncInfo {
  mode: string;
  machine_id: string;
  sync_enabled: boolean;
  sync_interval: string;
  stats: { table: string; total: number; unsynced: number }[];
  last_push?: string;
  last_pull?: string;
  clients?: SyncClientInfo[];
}

export async function fetchCloudStats(): Promise<StatsResponse | null> {
  try {
    const res = await authFetch(`${BASE}/api/sync/cloud-stats`);
    if (!res.ok) return null;
    return res.json();
  } catch {
    return null;
  }
}

export interface ProjectInfo {
  name: string;
  observation_count: number;
}

export async function fetchProjects(): Promise<ProjectInfo[]> {
  const res = await authFetch(`${BASE}/api/projects`);
  return res.json();
}

export async function fetchHealth(): Promise<HealthResponse> {
  const res = await fetch(`${BASE}/api/health`);
  return res.json();
}

export async function search(q: string, project?: string, limit = 10): Promise<SearchResponse> {
  const params = new URLSearchParams({ q, limit: String(limit) });
  if (project) params.set('project', project);
  const res = await authFetch(`${BASE}/api/search?${params}`);
  return res.json();
}

export async function searchTimeline(project: string, from?: string, to?: string, limit = 50): Promise<SearchResponse> {
  const params = new URLSearchParams({ project, limit: String(limit) });
  if (from) params.set('from', from);
  if (to) params.set('to', to);
  const res = await authFetch(`${BASE}/api/search/timeline?${params}`);
  return res.json();
}

export async function listObservations(project: string, type?: string, limit = 50): Promise<SearchResponse> {
  const params = new URLSearchParams({ project, limit: String(limit) });
  if (type) params.set('type', type);
  const res = await authFetch(`${BASE}/api/observations?${params}`);
  return res.json();
}

export interface Summary {
  id: number;
  memory_session_id: string;
  project: string;
  request?: string;
  investigated?: string;
  learned?: string;
  completed?: string;
  next_steps?: string;
  notes?: string;
  created_at: string;
}

export interface SummariesResponse {
  summaries: Summary[] | null;
  total: number;
}

export async function listSummaries(project: string, limit = 20): Promise<SummariesResponse> {
  const params = new URLSearchParams({ project, limit: String(limit) });
  const res = await authFetch(`${BASE}/api/summaries?${params}`);
  return res.json();
}

export interface Prompt {
  id: number;
  content_session_id: string;
  project: string;
  prompt: string;
  prompt_number: number;
  created_at: string;
}

export interface PromptsResponse {
  prompts: Prompt[] | null;
  total: number;
}

export async function listPrompts(project: string, limit = 50): Promise<PromptsResponse> {
  const params = new URLSearchParams({ project, limit: String(limit) });
  const res = await authFetch(`${BASE}/api/prompts?${params}`);
  return res.json();
}

export async function fetchSyncInfo(): Promise<SyncInfo> {
  const res = await authFetch(`${BASE}/api/sync/info`);
  return res.json();
}

export interface LogEntry {
  time: string;
  level: string;
  message: string;
  raw: string;
}

export interface LogsResponse {
  entries: LogEntry[];
  total: number;
}

export async function fetchLogs(level?: string, tail?: number): Promise<LogsResponse> {
  const params = new URLSearchParams();
  if (level) params.set('level', level);
  if (tail) params.set('tail', String(tail));
  const res = await authFetch(`${BASE}/api/logs?${params}`);
  return res.json();
}

export interface Settings {
  worker_port: number;
  data_dir: string;
  log_level: string;
  database_url: string;
  gemini_api_key: string;
  gemini_model: string;
  gemini_embedding_model: string;
  gemini_embedding_dims: number;
  context_observations: number;
  context_full_count: number;
  context_session_count: number;
  skip_tools: string;
  allowed_projects: string;
  ignored_projects: string;
  sync_enabled: boolean;
  sync_url: string;
  sync_interval: string;
  machine_id: string;
}

export async function fetchSettings(): Promise<Settings> {
  const res = await authFetch(`${BASE}/api/settings`);
  return res.json();
}

export async function updateSettings(partial: Partial<Settings>): Promise<Settings> {
  const res = await authFetch(`${BASE}/api/settings`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json', ...authHeaders() },
    body: JSON.stringify(partial),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: 'Unknown error' }));
    throw new Error(err.error || `HTTP ${res.status}`);
  }
  return res.json();
}

// ── Graph backfill ──────────────────────────────────────────────────────────

export interface BackfillSlackResponse {
  job_id: number;
  status: string;
  channel_id: string;
  oldest_ts: string;
  estimated_months: number;
}

export async function backfillSlack(channelId: string, months: number): Promise<BackfillSlackResponse> {
  const res = await authFetch(`${BASE}/api/graph/backfill/slack`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...authHeaders() },
    body: JSON.stringify({ channel_id: channelId, months }),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: 'Unknown error' }));
    throw new Error(err.error || `HTTP ${res.status}`);
  }
  return res.json();
}

// ── Graph jobs ───────────────────────────────────────────────────────────────

export interface JobRow {
  id: number;
  type: string;
  priority: number;
  payload: unknown;
  available_at: string;
  attempts: number;
  status: string;
  last_error: string;
}

export interface JobsListResponse {
  queue_depth: Record<string, number>;
  oldest_queued_age_s: number;
  jobs: JobRow[];
}

export async function listJobs(status?: string, type?: string, limit = 50): Promise<JobsListResponse> {
  const params = new URLSearchParams({ limit: String(limit) });
  if (status) params.set('status', status);
  if (type) params.set('type', type);
  const res = await authFetch(`${BASE}/api/graph/jobs?${params}`);
  return res.json();
}

export async function retryJob(id: number): Promise<void> {
  await authFetch(`${BASE}/api/graph/jobs/${id}/retry`, { method: 'POST', headers: authHeaders() });
}

export async function deleteJob(id: number): Promise<void> {
  await authFetch(`${BASE}/api/graph/jobs/${id}`, { method: 'DELETE', headers: authHeaders() });
}

// ── Graph search / resolve / node ────────────────────────────────────────────

export interface GraphNode {
  id: string;
  type: string;
  title?: string;
  url?: string;
  body?: string;
  scope?: string;
  score?: number;
  score_breakdown?: Record<string, number>;
  author?: string;
  updated_at?: string;
}

export interface GraphSearchResponse {
  results: GraphNode[];
  query: string;
  total: number;
}

export async function graphSearch(query: string, types?: string[], limit = 20): Promise<GraphSearchResponse> {
  const params = new URLSearchParams({ q: query, limit: String(limit) });
  if (types && types.length > 0) params.set('types', types.join(','));
  const res = await authFetch(`${BASE}/api/graph/search?${params}`);
  return res.json();
}

export interface ResolveArtifact {
  node_id: string;
  type: string;
  title?: string;
  url?: string;
  body?: string;
  author?: string;
  score?: number;
  hop: number;
}

export interface ResolveTrace {
  expanded_nodes: number;
  after_acl: number;
  took_ms: number;
  cache_misses?: string[];
}

export interface GraphResolveResponse {
  artifacts: ResolveArtifact[];
  trace: ResolveTrace;
}

export async function graphResolve(
  seeds: string[],
  query?: string,
  depth = 2,
  budgetTokens = 4000,
): Promise<GraphResolveResponse> {
  const res = await authFetch(`${BASE}/api/graph/resolve`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...authHeaders() },
    body: JSON.stringify({ seeds, query, depth, budget_tokens: budgetTokens }),
  });
  return res.json();
}

export interface GraphNodeDetail {
  id: string;
  type: string;
  title?: string;
  url?: string;
  body?: string;
  scope?: string;
  author?: string;
  updated_at?: string;
  metadata?: Record<string, unknown>;
}

export async function graphNode(url?: string, id?: string): Promise<GraphNodeDetail> {
  const params = new URLSearchParams();
  if (url) params.set('url', url);
  if (id) params.set('id', id);
  const res = await authFetch(`${BASE}/api/graph/node?${params}`);
  return res.json();
}

export interface GraphNeighbor {
  node: { node_id: string; type: string; url: string; title: string };
  edge: { kind: string };
  hop: number;
}

export async function graphSlackUsers(): Promise<Record<string, string>> {
  const res = await authFetch(`${BASE}/api/graph/slack-users`);
  return res.json();
}

export async function graphNeighbors(id: string, depth = 1): Promise<GraphNeighbor[]> {
  // Keep ':' literal — the chi path param doesn't decode %3A, so node ids like
  // "jira:PAY-2190" / "slack:C..:ts" must keep their colons unencoded.
  const seg = encodeURIComponent(id).replace(/%3A/gi, ':');
  const res = await authFetch(`${BASE}/api/graph/node/${seg}/neighbors?depth=${depth}`);
  const data = await res.json();
  return data.neighbors ?? [];
}
