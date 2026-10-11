import { test } from 'node:test';
import assert from 'node:assert/strict';

const store = new Map<string, string>([['agent_mem_api_key', 'k']]);
Object.assign(globalThis, {
  localStorage: { getItem: (k: string) => store.get(k) ?? null },
  window: { dispatchEvent: () => true },
});

let seen: Headers | undefined;
globalThis.fetch = (async (_url: unknown, init?: RequestInit) => {
  seen = new Headers(init?.headers);
  return new Response('{}');
}) as typeof fetch;

const { authFetch } = await import('../src/api.ts');

test('authFetch sends the admin header and the bearer key', async () => {
  await authFetch('/api/graph/search');
  assert.equal(seen?.get('X-Graph-Admin'), '1');
  assert.equal(seen?.get('Authorization'), 'Bearer k');
});

test('a caller cannot drop or override the admin header', async () => {
  for (const headers of [{ 'x-graph-admin': '0' }, new Headers({ 'X-Graph-Admin': '' }), [['X-Graph-Admin', 'no']] as [string, string][]]) {
    await authFetch('/x', { headers });
    assert.equal(seen?.get('X-Graph-Admin'), '1');
  }
});

test('caller headers are kept', async () => {
  await authFetch('/x', { headers: { 'Content-Type': 'application/json' } });
  assert.equal(seen?.get('Content-Type'), 'application/json');
  assert.equal(seen?.get('X-Graph-Admin'), '1');
});
