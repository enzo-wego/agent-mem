#!/usr/bin/env bash
# Real-page check of Search.tsx Slack-link chips against a mock API (port 5199).
set -u
cd "$(dirname "$0")/.."
SESSION=h07v-links
LOG=$(mktemp)
npx vite --config scripts/vite.mock.config.ts >"$LOG" 2>&1 &
SRV=$!
cleanup() { agent-browser --session "$SESSION" close >/dev/null 2>&1; kill "$SRV" 2>/dev/null; wait "$SRV" 2>/dev/null; }
trap cleanup EXIT
for _ in $(seq 1 60); do curl -sf http://localhost:5199/ >/dev/null && break; sleep 0.5; done
curl -sf http://localhost:5199/ >/dev/null || { echo "mock server did not start"; cat "$LOG"; exit 1; }

AB="agent-browser --session $SESSION"
$AB open http://localhost:5199/ >/dev/null || exit 1
$AB find role button click --name Search >/dev/null 2>&1 || $AB find text Search click >/dev/null || exit 1
$AB fill 'input[type=text]' 'https://wego.slack.com/archives/C012A121AQJ/p1783576586388629' >/dev/null || exit 1
$AB press Enter >/dev/null || exit 1
$AB wait '[data-linked-item]' >/dev/null || { echo "no linked items rendered"; exit 1; }
JSFILE=$(mktemp)
cat >"$JSFILE" <<'JS'
JSON.stringify([...document.querySelectorAll('[data-linked-item]')].map((el) => ({
  title: el.querySelector('h4').textContent,
  chips: [...el.querySelectorAll('[data-link-chip]')].map((c) => c.textContent),
  tooltips: [...el.querySelectorAll('[data-link-chip]')].map((c) => c.getAttribute('title')),
  why: el.querySelector('[data-link-why]')?.textContent ?? null,
  hop: el.querySelector('[data-hop]').textContent,
})))
JS
OUT=$($AB eval --stdin <"$JSFILE") || { echo "eval failed"; exit 1; }
echo "$OUT"
printf '%s' "$OUT" | node -e '
const assert = require("node:assert/strict");
let s = require("fs").readFileSync(0, "utf8").trim();
let items = JSON.parse(s);
if (typeof items === "string") items = JSON.parse(items);
const by = (t) => { const i = items.find((x) => x.title === t); assert.ok(i, "missing " + t); return i; };
assert.equal(items.length, 8);
const ref = by("Reference item"); assert.deepEqual(ref.chips, ["reference"]);
assert.deepEqual(by("Thread reply").chips, ["same thread"]);
const t = by("Topic with why"); assert.deepEqual(t.chips, ["same topic 0.91"]); assert.equal(t.tooltips[0], "refund dup"); assert.equal(t.why, "same mechanism");
const b = by("Topic bare"); assert.deepEqual(b.chips, ["same topic"]); assert.equal(b.why, null);
assert.deepEqual(by("Part of item").chips, ["epic"]);
assert.deepEqual(by("Mentions item").chips, ["mentions"]);
assert.deepEqual(by("Hop one no links").chips, []);
assert.deepEqual(by("Hop two item").chips, []);
assert.equal(by("Hop two item").hop, "hop 2");
for (const i of items) assert.match(i.hop, /^hop \d$/);
const order = items.map((i) => i.title);
assert.deepEqual(order, ["Reference item","Topic with why","Thread reply","Topic bare","Mentions item","Part of item","Hop one no links","Hop two item"]);
console.log("assertions ok");
' || exit 1
