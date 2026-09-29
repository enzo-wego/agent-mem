import { test } from 'node:test';
import assert from 'node:assert/strict';
import { jiraSyncState, type JiraSyncInput } from '../src/jiraSyncState.ts';

const now = new Date('2026-09-29T06:00:00Z');
const base: JiraSyncInput = {
  enabled: true,
  interval_minutes: 15,
  last_ok_at: '2026-09-29T05:50:00Z',
  last_error: '',
};

test('unavailable when the GET failed', () => {
  assert.equal(jiraSyncState(null, now), 'unavailable');
});

test('off when disabled', () => {
  assert.equal(jiraSyncState({ ...base, enabled: false, last_ok_at: null }, now), 'off');
});

test('never when last_ok_at is null', () => {
  assert.equal(jiraSyncState({ ...base, last_ok_at: null, last_error: 'cursor missing' }, now), 'never');
});

test('stale when last_ok_at is older than 3x interval', () => {
  assert.equal(jiraSyncState({ ...base, last_ok_at: '2026-09-29T04:00:00Z', last_error: 'boom' }, now), 'stale');
});

test('error when last_error is set', () => {
  assert.equal(jiraSyncState({ ...base, last_error: 'jira search 500' }, now), 'error');
});

test('ok when healthy', () => {
  assert.equal(jiraSyncState(base, now), 'ok');
});

test('exactly 3x interval is ok', () => {
  assert.equal(jiraSyncState({ ...base, last_ok_at: '2026-09-29T05:15:00Z' }, now), 'ok');
});

test('3x interval plus 1s is stale', () => {
  assert.equal(jiraSyncState({ ...base, last_ok_at: '2026-09-29T05:14:59Z' }, now), 'stale');
});
