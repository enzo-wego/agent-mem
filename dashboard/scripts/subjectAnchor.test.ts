import { test } from 'node:test';
import assert from 'node:assert/strict';
import { subjectAnchors, mentionsAnchor } from '../src/subjectAnchor.ts';

const queries = [
  'Jaywan card scheme Checkout.com AED',
  'Jaywan BIN list card detection',
  'Jaywan logo accepted cards strip',
  'Jaywan Apple Pay AEP issue',
  'Jaywan go-live UAE payment method',
];

test('anchors for PAY-2397 queries', () => {
  assert.deepEqual(subjectAnchors(queries), ['jaywan']);
});

test('mentionsAnchor filters generic threads', () => {
  const a = subjectAnchors(queries);
  assert.equal(mentionsAnchor('Checkout payment performance review, Oct vs Sep 2026', a), false);
  assert.equal(mentionsAnchor('Whether Jaywan needs MADA-style udf in payment request', a), true);
});

test('no shared word yields no anchors', () => {
  const a = subjectAnchors(['alpha beta', 'gamma delta', 'epsilon zeta']);
  assert.deepEqual(a, []);
  assert.equal(mentionsAnchor('alpha', a), false);
});
