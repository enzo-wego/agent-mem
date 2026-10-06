import assert from 'node:assert/strict'
import { linkChips } from '../src/pages/linkChips.ts'

assert.deepEqual(linkChips(undefined), { chips: [] })
assert.deepEqual(linkChips([]), { chips: [] })
assert.deepEqual(linkChips([{ kind: 'REFERENCES' }, { kind: 'REFERS_TO' }]), { chips: [{ label: 'reference' }] })
assert.deepEqual(linkChips([{ kind: 'REFERS_TO' }]), { chips: [{ label: 'reference' }] })
assert.deepEqual(linkChips([{ kind: 'THREAD' }]), { chips: [{ label: 'same thread' }] })
assert.deepEqual(
  linkChips([{ kind: 'SAME_TOPIC', confidence: 0.9, topic: 'refund dup', why: 'same mechanism' }]),
  { chips: [{ label: 'same topic 0.90', title: 'refund dup' }], why: 'same mechanism' },
)
assert.deepEqual(linkChips([{ kind: 'SAME_TOPIC', confidence: 0.91 }]), { chips: [{ label: 'same topic 0.91' }] })
assert.deepEqual(linkChips([{ kind: 'SAME_TOPIC', topic: 't' }]), { chips: [{ label: 'same topic', title: 't' }] })
assert.deepEqual(linkChips([{ kind: 'SAME_TOPIC' }]), { chips: [{ label: 'same topic' }] })
assert.deepEqual(linkChips([{ kind: 'PART_OF' }]), { chips: [{ label: 'epic' }] })
assert.deepEqual(linkChips([{ kind: 'MENTIONS' }]), { chips: [{ label: 'mentions' }] })
assert.deepEqual(linkChips([{ kind: 'BLOCKED_BY' }]), { chips: [{ label: 'blocked by' }] })
// table order regardless of input order
assert.deepEqual(
  linkChips([{ kind: 'MENTIONS' }, { kind: 'PART_OF' }, { kind: 'SAME_TOPIC', confidence: 0.5 }, { kind: 'THREAD' }, { kind: 'REFERENCES' }]).chips.map((c) => c.label),
  ['reference', 'same thread', 'same topic 0.50', 'epic', 'mentions'],
)
