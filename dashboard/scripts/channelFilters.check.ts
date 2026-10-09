import assert from 'node:assert/strict'
import { cfgFromState, channelLabel, rulesFromCfg } from '../src/channelFilters.ts'

const cfg = { ignore: ['C1'], drop_authors: { CUV: ['B1', 'U2'] }, names: { C1: 'one' } }
const rules = rulesFromCfg(cfg)
assert.equal(rules.length, 1)
assert.equal(rules[0].dropAuthors, 'B1, U2')
const out = cfgFromState(cfg.ignore, rules)
assert.deepEqual(out.drop_authors, { CUV: ['B1', 'U2'] })
assert.equal('names' in out, false)
assert.equal('names' in cfgFromState(['C1'], rulesFromCfg(cfg)), false)

rules[0].dropAuthors = '  '
assert.deepEqual(cfgFromState([], rules).drop_authors, {})

const chans = [{ channel_id: 'A', name: 'alpha' }, { channel_id: 'E', name: '' }]
assert.equal(channelLabel('A', chans, { A: 'x' }), 'alpha')
assert.equal(channelLabel('B', chans, { B: 'beta' }), 'beta')
assert.equal(channelLabel('E', chans, { E: '' }), 'unknown channel')
assert.equal(channelLabel('Z', chans, undefined), 'unknown channel')
console.log('ok')
