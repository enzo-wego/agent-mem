# R1b: lead-mode eval snapshot and relation labels for the wrong-item detectors

Date: 2026-10-12. Owner: Enzo. Status: draft r4 (after pi r1-r3 CHANGES REQUIRED; r4 fixes r3's R1-F3 and R1-F10).
REVIEWER: pi (auto: GPT weekly 53% left)
PAGE: https://claude.ai/artifact/TwHSuYR4kVThmJf5SqQbdy
STATUS: approved (Enzo, 2026-10-12 19:37 SGT)
Repo: agent-mem (`/Users/enzo/go/src/github.com/agent-mem`), base `origin/main` 288d1a4.
Parent: p-agent `docs/ai/2026-10-12-graph-context-in-bot.md` (umbrella), section "Round split":
R1 includes "`eval prepare --refresh` with new draft labels aimed at wrong items (same
symptom, different case), pi-checked on masked sheets". R1a (lead mode) shipped as PR #99.
The next round, R1c (p-agent), runs detectors 1-2 offline on what this round produces. Its
checklist is p-agent `docs/reviews/2026-10-12-graph-detector-offline-test-r1.md` (R3, R6, R7,
R8 depend on this round). Review of r1: `docs/reviews/2026-10-12-graph-context-r1b-wrong-item-labels-r1.md`.

## What changed from r1 (one line per pi finding)

- F1, F6, F7, F8: existing case files and their retrieval labels are **not touched**. The new
  ground truth is a separate tracked file, `relation_labels.json`, with its own rubric
  (`same_case | related_context | different_case | insufficient`). No freeze-test move, no
  digest invalidation, no label collisions, no change to relevance metrics.
- F2, F4: masking becomes per-case pseudonyms (`ref-1`, `ref-2`, `person-1` …) shared by
  the query and every row of that case, applied to full text before truncation, with an
  explicit grammar. Residual free-text names are an owner decision on the approval page.
- F3: sheets and detectors use the exact lead response text (title, `lead_summary`), saved
  once in a private snapshot outside the repo; eligibility metadata (ids, enums, numbers) is
  saved in the tracked replay.
- F5: private output directory lifecycle, exclusive file creation, retention and deletion.
- F9: the hub checkout is never written. Part B runs in the worktree.
- F10: replay `status` stays an int; a new `outcome` enum; `label-sheet` default stays the
  union of all variants.
- F11: Part C narrowed to what the evidence can show; residuals reported as unexplained.
- F12: gates and acceptance rewritten as exact commands and assertions.

## What changed from r2 (pi r2 open items)

- R1-F2: `AddPerson(slackID, name)` links id and name; the snapshot keeps `author` and
  `author_slack_id`; identity limits stated (A5).
- R1-F4: protected-token shield first; labelled refs need a digit; multi-word labels;
  upstream-cut tail rule; real `capRunes` boundary test (A5, A8).
- R1-F3: `eligibility_sha256` binds the metadata; exhaustive link and author buckets; global
  target is sampling only (Cohort buckets, A2, A7).
- R1-F10: strict lead decoding with required shape; lead readers refuse legacy, failed or
  incomplete data; unknown variants rejected (A2, A6).
- R1-F12: all runs from the worktree with `$PRIV/agent-mem` and `--base-url
  http://localhost:34567`; warm-up counted separately; `PART_B_BASE`; dry-run is a named Go
  test (B1, Acceptance).
- R2-F1: `relation-check` requires exact case and item coverage, rehashes case files,
  rejects duplicate keys and decisive disputed rows, checks seeds (A7).
- R2-F2: models write rows only; `relation-assemble` computes the header and the merge;
  pi reviews independently instead of editing the draft (A7, B3-B6).

## What changed from r3 (pi r3: two open entries)

- R1-F3 and R1-F10: shared `validateLeadReplay` (A2 end) runs first in the lead sheet
  reader and both relation commands: strict outcome/status/items checks and a recomputed
  `eligibility_sha256`; metadata-only and outcome-only mutation regressions in A8.

## Goal

1. A fourth eval variant, `resolve-lead`, measures what the bot receives in lead mode.
2. One `eval prepare` run records, for each case, the lead response: ids and eligibility
   metadata in the tracked `replay.json`; title and `lead_summary` text in a private snapshot
   outside the repo.
3. Masked label sheets built from that snapshot, with per-case pseudonyms, so pi reads
   masked text that still shows whether two references are the same or different.
4. A tracked `relation_labels.json`: for every non-seed lead root of every case, whether it
   is the same case, useful related context, a different case, or not decidable. This is the
   ground truth R1c needs. Target: enough `different_case` items to test detectors.
5. Measured answers on lead-vs-plain counts and `authored_by_bot=unknown`, limited to what
   the data shows.

## Non-goals

- No server, handler, ingest or migration change. No deploy. CLI only.
- No edits to `testdata/eval/graph_context/cases/*.yaml` (labels, reasons, seeds, requests),
  so `TestEvalCaseLabelsUnchangedFromBase` and `cases_sha256` stay valid.
- No fix for `authored_by_bot=unknown`. Part C files a bd issue.
- No detector code, union scoring or judge (R1c).
- No customer text in any repo, bd issue, Slack message, approval page or command stdout.

## Facts (checked 2026-10-12, agent-mem 58b9624 = 288d1a4 + Enzo's local bd commit)

- Subcommands (`cmd/agent-mem/eval.go:45-79`): `import-probes`, `relevance [--quiet
  --base-url --dry-run --json]`, `prepare --base-url [--refresh]`, `label-sheet [case-id]
  --out <dir outside repo> [--all]`.
- Variants `{"resolve-admin","resolve-asker","search-hybrid"}` (`eval_common.go:25`); headers
  from `evalVariantHeaders` (`:82-91`); every request carries `X-Graph-Caller: eval` and
  `X-Graph-No-Side-Effects: 1` (`evalRequestHeaders`). Nothing sends `X-Graph-Lead`.
- `prepare` (`eval_prepare.go:95-212`): read-only DB transaction; canonicalises seeds;
  replays every variant; writes `canonical_seeds:` lines, `node_meta.json`, `replay.json`
  (temp file + rename, 0644, all-or-nothing). `replayVariant.Status` is an int HTTP status
  (`eval_prepare.go:23-27`). `--refresh` keeps existing `node_meta.json` entries; without it
  `node_meta.json` is rebuilt (`:154-176`). Reads `DATABASE_URL` from `./.env`, else the
  process env (`eval_common.go:176`); `AGENT_MEM_API_KEY` from the env first, then `.env`.
- `replay.json` stores `cases_sha256` = hash of full case bytes (`eval_common.go:70-77`);
  `label-sheet` refuses on mismatch (`eval_prepare.go:305-306`).
- `label-sheet` (`eval_prepare.go:279-369`): candidates are the deduplicated union of all
  variants' returned ids (`:337-344`); text from `readSheetNode` (`:388-405`), which reads
  the raw title and the first 300 chars of `artifact_index.summary`, else body, truncated in
  SQL; header also prints use case, source thread and query (`:347-348`); dir
  `MkdirAll 0700`, files `os.WriteFile 0600` (`:359-364`); refuses `--out` inside the repo.
  Sheet row heading today: `## <node_id>` (asserted in `eval_harness_test.go:555-558`).
- Lead mode text (`internal/graph/handlers/lead_resolve.go:181-190`, `lead.go:68-74,288-326`):
  title falls back to the Slack thread summary title; `lead_summary` = artifact summary,
  masked (non-admin) then capped at 600 runes, no body fallback. Items are thread roots
  (replies fold under the root, best reply as `focus_ts`); roots missing, deleted, out of
  scope, ACL-invisible or outside the epic filter are dropped; seed groups always kept, then
  others up to `max_items` (default 30). Each item has `hop`, `links[{kind, confidence, from,
  to, …}]`, `authored_by_bot`, `bot_in_thread`, `last_activity_at`.
- `authored_by_bot` (`lead.go:199-207,288-310`): `unknown` when the node has no person row
  (`author_person_id` null); `bot` when `people.is_bot` or the person's Slack id is in
  `graph.answer_bots` (one row, EnzoBot); `human` otherwise.
- `redact.Mask` (`internal/redact/redact.go:67`) masks emails, phones, card-like numbers;
  it deliberately leaves labelled booking refs of 5-20 chars (`redact.go:46-48`).
- Cases: 20 files, sorted ids `case-01`..`case-14`, `probe-a`..`probe-f`. Query at
  `requests["resolve-asker"].body.query` (YAML anchor shared with other variants).
  `needs_context: no`: case-03, case-14. Existing resolve `must_not`: case-01 only (4 ids,
  all replies; lead mode never returns replies).
- Tests: `make test-db` runs `go test -p=1 -count=1 -json ./internal/graph/...
  ./cmd/agent-mem/...` against throwaway pgvector containers (Makefile:42-64). It does not
  run `./internal/redact/...`.
- Worktree `/Users/enzo/go/src/github.com/agent-mem-wt-lead` exists, detached at 288d1a4.
  Hub checkout `/Users/enzo/go/src/github.com/agent-mem` runs the worker and holds Enzo's
  unpushed bd commit: never written by this round.

## Relation rubric (the ground truth R1c scores against)

For one case: the request is the case's query and seeds. For one non-seed lead root:

| relation | meaning | synthetic example (request: "payment ref-1 failed with 3DS timeout at partner-1 on 10-03") |
|---|---|---|
| `same_case` | about the same occurrence: same ref/incident/ticket, or a thread/ticket/PR that is the work on it | thread "ref-1 3DS timeout, retried", or the Jira ticket opened for ref-1 |
| `related_context` | a different occurrence or topic that would still help answer (same component, known precedent, runbook, the fix PR for this class of failure) and does not claim to be this case | "3DS timeouts at partner-1 last month: root cause was X", a runbook |
| `different_case` | same kind of symptom or shared words, but a different occurrence, presented in a way a reader could mistake for this case, and not useful as precedent | "ref-2 failed with 3DS timeout at partner-1 on 09-14" with no shared cause stated; an unrelated thread that only shares "timeout" |
| `insufficient` | the title and `lead_summary` do not let a reader decide (empty summary, truncated, ambiguous) | empty summary and a generic title |

Reason codes (enum, one per item): `same_ref`, `same_ticket_or_pr`, `work_on_case`,
`precedent`, `same_component`, `runbook`, `different_ref`, `different_incident`,
`shared_words_only`, `empty_evidence`, `ambiguous`, `reviewers_disagree`.

Mapping for R1c (stated here so R1c needs no reinterpretation):
- detector should flag: `different_case`;
- detector should not flag: `same_case` and `related_context`, reported as two separate
  denominators (`related_context` rejection is an optional-context rejection rate, not a
  proven false positive);
- `insufficient`: excluded from every truth denominator, counted as coverage loss.

The existing `labels.resolve`/`labels.search` (`must/may/must_not`) are untouched and keep
their retrieval meaning. `relation_labels.json` does not replace them.

## Cohort buckets (reported by relation-check, consumed by R1c)

Every non-seed lead item gets exactly one **link bucket**, from its `links[]` (kinds and
confidence as returned):

1. `structural`: any link of kind PART_OF, REFERS_TO or REFERENCES (mixed items with
   SAME_TOPIC or THREAD too land here).
2. `same_topic_hi`: else any SAME_TOPIC link with confidence >= 0.9.
3. `same_topic_lo`: else any SAME_TOPIC link with confidence < 0.9.
4. `other_link`: else any other kind (THREAD and anything not listed above).
5. `no_link`: `links` empty.

and exactly one **author bucket**: `authored_by_bot` value `human`, `bot` or `unknown`.

The global readiness numbers (below) are a sampling target only. R1c decides which buckets
form its judge cohort and must report per bucket; a bucket with fewer than 5 decisive
`different_case` items is inconclusive for that bucket, whatever the global totals say.

## Part A: code (omp worker, worktree, one PR)

Worktree setup (worker): `git -C /Users/enzo/go/src/github.com/agent-mem-wt-lead fetch origin
&& git -C /Users/enzo/go/src/github.com/agent-mem-wt-lead checkout -b feat/eval-lead-snapshot
origin/main`. Work only there.

A1. **Variant `resolve-lead`.** Append to `evalVariants`. Request body: a deep copy of the
   case's `resolve-asker` body (never mutate the shared map; the YAML anchor makes variant
   bodies share values) with `include_bodies: false` and `max_items: 30` set. Headers: the
   `resolve-asker` headers plus `X-Graph-Lead: 1`; never `X-Graph-Admin`. Any code that needs
   the case query for this round reads `requests["resolve-asker"].body.query`; do not reuse
   `caseQuery` (`eval_prepare.go:372-383`), which prefers the admin query.
A2. **Replay schema, additive; strict lead decoding.** `replayVariant` keeps `status int`;
   add `outcome string` (`ok`, `lead_off`, `http_error`, `transport_error`, `decode_error`).
   Legacy replays (no `outcome`) still decode for the three old variants; empty `outcome`
   with 2xx status reads as `ok` for those variants only.
   A `resolve-lead` response is `ok` only if: HTTP 2xx; no `X-Graph-Lead: off` header (that
   gives `lead_off`); the JSON has an `artifacts` key holding an array (empty allowed); every
   element has non-empty string `node_id`, integer `hop`, `links` absent or an array whose
   elements have string `kind`, numeric `confidence` (absent reads 0), and string `from`/`to`;
   `authored_by_bot` in {`human`,`bot`,`unknown`}; node ids unique in the response. Anything
   else is `decode_error`. Decode into a dedicated struct with presence checks
   (`json.RawMessage` or pointer fields), not the generic parser at `eval_common.go:242-284`,
   which accepts `{}`.
   For `resolve-lead` only, the replay variant adds `items: [{id, hop, links: [{kind,
   confidence, from, to}], authored_by_bot, bot_in_thread, has_title, summary_runes,
   last_activity_at}]` in response order (ids, enums, numbers; never text), and the replay
   top level adds `eligibility_sha256`: SHA-256 of the canonical JSON (keys sorted, no
   whitespace) of `{case_id: items}` for all cases, sorted by case id.
   `prepare` fails the whole run (no files written, nothing renamed into place) when any
   case's `resolve-lead` outcome is not `ok`; stderr gets one fixed line,
   `prepare: lead_<outcome> <case_id>`.
   **Strict lead-replay validation (shared).** One function, `validateLeadReplay`, used by
   `label-sheet --variant resolve-lead`, `relation-assemble` and `relation-check` before
   anything else: every one of the 20 cases has a `resolve-lead` variant; its `outcome` is
   exactly `ok` (missing or any other value refuses; no legacy fallback for lead) and its
   `status` is 2xx; `items` is present (an empty array is allowed) and every item passes the
   A2 shape rules with unique ids; then it **recomputes** `eligibility_sha256` from the loaded
   items with the A2 canonical form and refuses unless it equals the stored replay value.
   Readers that also hold a stored copy elsewhere (snapshot header, relation-labels header)
   compare against the recomputed value too. Fixed categories: `lead_missing`,
   `lead_outcome`, `lead_items`, `eligibility_mismatch`. R1c must call `relation-check` (or
   the same validation) before scoring.
A3. **Private snapshot.** `prepare --snapshot <dir>`, required when `resolve-lead` is in
   the variant list. `<dir>` must not exist; its parent, resolved with
   `filepath.EvalSymlinks`, must be outside the repo; created with `os.Mkdir(dir, 0700)`.
   One file `snapshot.json`, opened `O_CREATE|O_EXCL|O_WRONLY` with mode 0600, after an
   `os.Lstat` check that nothing exists at the path:
   `{prepared_at, cases_sha256, eligibility_sha256, cases: {case_id: {query, items:
   {node_id: {title, lead_summary, author, author_slack_id}}}}}`, values exactly as the
   server returned them (`author`, `author_slack_id` may be empty). `replay.json` records
   `snapshot_sha256` (hex SHA-256 of the file bytes). The snapshot is written last, after
   every case succeeded; on any later failure the snapshot dir is removed.
   `build_rev`: the worker checks whether the server exposes its build revision
   (`rg -n "BUILD_REV" cmd internal`); if it does, record it, else record the `BUILD_REV`
   env var, else `unknown`. Document which in the eval doc.
A4. **`relevance` scores `resolve-lead`** against `labels.resolve` with the existing metric
   code, plus per case `roots` and `seed_groups`. Reply-only `must` ids (per `node_meta.json`
   `is_reply`) that lead mode cannot return are reported per case as `unreachable_must` and
   excluded from the lead recall denominator only. A run is failed when its `outcome` is not
   `ok`. The warm-up request gets the same outcome classification. `--quiet` adds per
   variant `warmup_ok/warmup_total` and `runs_ok/runs_total` (measured runs only, 10 per
   case).
A5. **Pseudonymizer** in `internal/redact` (new file `pseudonym.go`; `Mask` unchanged; the
   email/phone/card regexes may be shared within the package):
   - `NewPseudonymizer()`: in-memory alias map for one case, never written or logged.
   - `AddPerson(slackID, displayName string)`: links a Slack user id and a display name to
     one `person-N` alias. Either may be empty.
   - `Apply(s string) string`, steps in this order on the full string:
     1. **Shield** protected tokens so later steps cannot match inside them: Jira keys
        `\b[A-Z][A-Z0-9]+-\d+\b`, `[A-Za-z0-9_.-]+#\d+`, ISO dates `\d{4}-\d{2}-\d{2}`,
        times `\d{2}:\d{2}(:\d{2})?`.
        **code-r1 owner decision (Enzo, 2026-10-12):** a label word (step 6, plus its optional
        `ref`/`reference`/`id`/`no`/`number` word) beats every shield: the token after it is
        masked as `ref-N` even if it looks like a Jira key, `repo#n`, date or time, so labelled
        refs are extracted before the shields; the Jira-key shield applies only to keys whose
        numeric part has at most 6 digits; date and time shields need non-digit boundaries on
        both sides. `payment PAY-2420` and `ref 2026-10-12` therefore mask the token, while
        `PAY-2420`, `payments#2424`, `2026-10-12` and `18:30` alone stay readable.
     2. Slack mentions `<@([UW][A-Z0-9]+)(\|[^>]*)?>` → the alias of that id (new
        `person-N` if unknown).
     3. Registered display names: `@Name` or whole-word `Name`, case-insensitive →
        that person's alias.
     4. Other `@` + 1-3 capitalised words → `person-N` keyed by the lowercased words.
     5. Emails → `email-N`; phones → `phone-N`; card-like numbers → `card-N`.
     6. Labelled refs: label word (`booking`, `payment`, `order`, `pnr`, `bid`, `txn`,
        `transaction`, `ref`, `reference`; case-insensitive, whole word), optionally followed
        by one of the words `ref`, `reference`, `id`, `no`, `number`, then optional `[:#=]`
        and spaces, then a token `[A-Za-z0-9][A-Za-z0-9-]{3,39}` **that contains at least
        one digit** → label words kept, token → `ref-N`. A token with no digit is not a
        ref, so prose like "payment failed" is untouched.
     7. Unlabelled refs: tokens bounded by `[^A-Za-z0-9_-]` or string ends (so inside URLs)
        that are 10+ chars mixing letters and digits, or 8+ digits; a hyphenated compound
        token qualifies if, with hyphens removed, it meets either rule → `ref-N`.
     8. Upstream-cut tail: if the input ends with `…`, the last token before it (`[A-Za-z0-9-]+`,
        any length) that contains a digit → `ref-N`. Reason: the server caps `lead_summary`
        at 600 runes (`lead.go:68-74`), which can leave a short fragment of a longer ref.
     Finally unshield, then run `Mask` as a second pass.
   - Alias identity: equal normalised values (lowercase, `-` and spaces removed) map to one
     alias per namespace (`person`, `email`, `phone`, `card`, `ref`). A person seen as
     `<@U1>`, `@Ann Lee` and `Ann Lee` gets one alias only when `AddPerson("U1","Ann Lee")`
     was called (author fields from the snapshot). Mentions of non-authors cannot be linked
     to their names and get separate aliases; two different people with the same display
     name share one alias. Both are accepted limits: they lose some evidence, never expose
     a value.
A6. **`label-sheet --variant`.** Default `all`: today's union of all variants, today's
   output except the row heading, which becomes `## [r<k>] <node_id>` (update the assertion
   in `eval_harness_test.go:555-558`). `--variant resolve-lead` requires `--mask` and
   `--snapshot <dir>` and refuses (fixed category on stderr, exit 1) unless: `replay.json`
   has `resolve-lead` with outcome `ok` and `items` for every case; `snapshot_sha256`,
   `eligibility_sha256` and `cases_sha256` all match (case bytes rehashed now); every replay
   item has a snapshot row. Any other `--variant` value is rejected. Per case one
   Pseudonymizer: `AddPerson(author_slack_id, author)` for every snapshot item first, then
   the query, use case and every row's title and `lead_summary` pass through `Apply`, full
   string first, then cut to title 200 / summary 600 runes. `source_thread` is replaced by
   the case id. Seeds (hop 0) are listed in a separate "Seeds" block, masked the same way,
   with no `[r<k>]` id. Rows: `## [r<k>] <node_id>`, then `bucket`, `hop`, `authored_by_bot`,
   masked title, masked summary. `--out`: must not exist, parent outside the repo,
   `os.Mkdir 0700`, files `O_CREATE|O_EXCL` 0600 after `Lstat`. Also writes
   `<out>/manifest.json`: `{case_id: [{r, node_id}]}` (ids only; the drafter's row
   inventory). Header: "MASKED: Slack mentions, author names, @-names, emails, phones, cards,
   refs with a digit (any token after a label word such as booking/payment/order). NOT masked: other names in prose, merchant/partner names. CONFIDENTIAL:
   do not commit or share." Errors are fixed categories on stderr, never content.
A7. **`eval relation-assemble`** and **`eval relation-check`**.
   - Row file format (what models produce): `{case_id: {node_id: {relation, reason}}}`.
   - `relation-assemble --draft <rows> --review <rows> [--owner <rows>] --out <file>`:
     reads `replay.json`, case files and `node_meta.json` from the data dir, computes the
     header itself (never from model output): `version: 1`, `adjudication: "r1b-v1"`,
     `cases_sha256`, `eligibility_sha256`, `snapshot_sha256`, `build_rev` (from
     `replay.json`), `input_mode: "lead:title+lead_summary"`. Merge per row: draft = review
     → that relation, `check: agree`; differ → `relation: insufficient`, `reason:
     reviewers_disagree`, `check: disputed`; an `--owner` row replaces either, `check:
     enzo`. A row missing from draft or review → `insufficient`, `empty_evidence`,
     `check: missing`. Output written `O_CREATE|O_EXCL` 0600. Unknown case or node ids in any
     input → exit 1 with fixed category.
   - `relation-check [--file <path>]` (default `testdata/eval/graph_context/relation_labels.json`)
     fails (exit 1, fixed category) on: invalid JSON or duplicate keys (decode with a
     duplicate-key-detecting decoder); missing or extra header field; `version != 1`;
     `input_mode` not as above; any of the three digests not equal to `replay.json`'s, or
     `cases_sha256` not equal to a fresh hash of the case files; the set of cases not equal
     to the 20 case ids; per case, the set of node ids not equal to the non-seed
     `resolve-lead` item ids in `replay.json` (empty map allowed only when that set is
     empty); a `relation` or `reason` outside the enums; `check` not in {agree, disputed,
     missing, enzo}; `check: disputed` or `missing` with a relation other than
     `insufficient`. Seeds are the hop-0 items; the check also fails with `seed_mismatch`
     when a canonical seed of the case (or its `thread_root` per `node_meta.json`) is not a
     hop-0 item. Post-merge fix: only seeds whose `node_meta.json` entry has `exists: true` are checked; a seed with `exists: false` or no entry is skipped.
     On success prints counts only: relation × link bucket, relation × author bucket, per
     case relation counts, cases with >= 1 decisive `different_case`, `insufficient` by
     reason.
A8. **Tests** (`cmd/agent-mem/eval_*_test.go`, `internal/redact/pseudonym_test.go`):
   - `TestEvalDryRunLeadVariant` (in-process, synthetic DataDir fixture of 2 cases): 4
     variants × 2 cases; lead lines show `X-Graph-Lead:1`, asker headers, no admin header,
     `include_bodies:false`, `max_items:30`; the `resolve-asker` body is unchanged after the
     lead body was built;
   - prepare with a fake server: `X-Graph-Lead: off`; HTTP 500 on one case; `{}`; artifacts
     absent; duplicate node id; item missing `hop`; bad `authored_by_bot` → each fails with
     its category and no file written, no snapshot dir left; valid empty `artifacts: []`
     succeeds; legacy replay decodes and works with `label-sheet --variant all` but is
     refused for `--variant resolve-lead`;
   - `eligibility_sha256` changes when only one link's confidence changes;
   - relevance on a fake lead response: `roots`, `seed_groups`, `unreachable_must`; a failed
     warm-up shows in `warmup_ok`; old variants' metrics match a golden output from before
     the change;
   - Pseudonymizer: `payment failed with timeout` unchanged; `booking reference AB1234` →
     `booking reference ref-1`; `PAY-2420` and `2026-10-12` alone unchanged (labelled: `payment ref-1`, `ref ref-1`);
     same ref twice → same alias; two refs → `ref-1`, `ref-2`;
     `https://x/y?id=AB12CD34EF56` masked inside the URL; `AB12-CD34EF56GH` masked;
     `payments#2424`, `18:30` kept; `AddPerson("U1","Ann Lee")` then `<@U1>`, `@Ann Lee`,
     `ann lee` → one alias; `<@W9ZZ|bo>` unregistered → its own alias; email/phone/card in
     separate namespaces; a 14-char ref cut by the real `capRunes(…,600)` from the handlers
     package (or a copy of its exact behaviour) and then passed to `Apply` → the tail
     fragment is `ref-N`;
   - label-sheet `resolve-lead` with sentinels planted in query, use case, title, summary,
     author name and author id: none appears in any output file; seeds block present without
     `[r<k>]`; manifest has ids only; existing `--out` dir, symlinked `--out`, symlinked file
     path, snapshot digest mismatch, eligibility mismatch, missing snapshot row → refused;
     file 0600, dir 0700;
   - relation-assemble: agree, differ, owner override, missing row, model-supplied header
     ignored;
   - `validateLeadReplay` regressions, each run through `label-sheet --variant resolve-lead`,
     `relation-assemble` and `relation-check` with otherwise valid labels and every stored
     digest left unchanged: only one link's confidence changed (0.95 → 0.50); only one
     endpoint changed; only one `authored_by_bot` changed; lead `outcome` missing,
     `lead_off`, `http_error`, `decode_error`; `items` missing; one item missing `hop` → all
     refuse with the fixed category; a valid empty `items: []` with consistent seeds passes; relation-check: valid file passes; empty `cases: {}`, one omitted case, one
     omitted item, extra item, seed item, stale case bytes, duplicate JSON key, disputed with
     `different_case`, wrong `input_mode`, eligibility mismatch → each fails with its
     category; a file with explicit `insufficient` rows passes and counts them;
   - stdout/stderr of `prepare`, `label-sheet`, `relation-assemble`, `relation-check` under
     planted sentinels contain no sentinel.
A9. **Docs.** The eval doc that mentions `label-sheet` (`rg -n "label-sheet" docs README*`):
   short paragraphs for `resolve-lead`, `--snapshot`, `--variant/--mask`, the two relation
   commands, the rubric and the buckets.

Gates (worker, in the worktree): `go vet ./...`, `go test ./internal/redact/...`,
`make test-db`, all green. Then `git diff --name-only origin/main | xargs rg -n
"t\.Skip|\.only\(|TODO"` prints nothing new. Commits on `feat/eval-lead-snapshot`, no push,
no PR. Conductor runs the deep review (fresh subagent with the `deep-review` skill), then
pushes, opens and merges the PR.

## Part B: snapshot and relation labels (conductor-run; worker applies the final file)

Private directory, created by the conductor:
`mkdir -p -m 700 /Users/enzo/.local/share/agent-mem-eval && PRIV=$(mktemp -d
/Users/enzo/.local/share/agent-mem-eval/r1b.XXXXXX)`. Never under a repo or the
scratchpad, never attached or published.

B1. Worktree: `git -C <wt> fetch origin && git -C <wt> checkout -b
   feat/eval-relation-labels-r1b origin/main` (Part A merged); record
   `PART_B_BASE=$(git -C <wt> rev-parse HEAD)`. `go build -o $PRIV/agent-mem
   ./cmd/agent-mem` in the worktree. One shell for all following CLI calls: `set -a; .
   /Users/enzo/go/src/github.com/agent-mem/.env; set +a` (never echoed), `cd <wt>`.
   Before: `$PRIV/agent-mem eval relevance --quiet --base-url http://localhost:34567 >
   $PRIV/before.txt` (old variants; lead variant fails until prepare has run, expected).
   Then `$PRIV/agent-mem eval prepare --base-url http://localhost:34567 --snapshot
   $PRIV/snap` (no `--refresh`: full `node_meta.json` rebuild gives fresh reply-to-root
   data for Part C; a deliberate change from the umbrella wording). If `git -C <wt> diff
   --name-only` lists any `cases/*.yaml`, stop and report.
B2. `$PRIV/agent-mem eval label-sheet --all --mask --variant resolve-lead --snapshot
   $PRIV/snap --out $PRIV/sheets`.
B3. Draft: a fresh Opus subagent gets `$PRIV/sheets/*.md`, `$PRIV/sheets/manifest.json`
   and the rubric only (no snapshot, no case files), and writes `$PRIV/draft.json` in the
   row format, one row per manifest entry.
B4. pi review: a pi pane gets the same inputs and writes `$PRIV/review.json` in the row
   format, independently (it is not shown the draft, so agreement means two independent
   reads). Prompt rules: masked text only, never write outside `$PRIV`, never commit,
   counts-only callback.
B5. Enzo (optional, does not block R1c): disputed rows, shown to him in a local file he
   opens on the Mac mini, never a published page; his answers in `$PRIV/owner.json`.
B6. `$PRIV/agent-mem eval relation-assemble --draft $PRIV/draft.json --review
   $PRIV/review.json [--owner $PRIV/owner.json] --out $PRIV/relation_labels.json`, then
   `relation-check --file $PRIV/relation_labels.json`. Worker copies that file to
   `testdata/eval/graph_context/relation_labels.json` in the Part B branch (replay.json and
   node_meta.json are already there from B1), runs `relation-check` and the gates, commits.
   Conductor reviews (ids-only diff), pushes, merges.
B7. Retention: `$PRIV/snap` stays until R1c's offline run finishes (R1c reads detector input
   text from it and checks `snapshot_sha256`); then the conductor deletes `$PRIV` and makes a
   best-effort deletion of the pi session file for B4 (path from pi's config; if not found,
   says so). Vendor-side retention (OpenAI for pi, Anthropic for Opus) is outside this
   repo's control and is not claimed.

Sampling target (reported, not enforced): at least 15 decisive `different_case` across at
least 6 cases, and at least 30 decisive `same_case` + `related_context`. Below that, Results
says so with real numbers. Per-bucket readiness is R1c's call (see Cohort buckets). If the
graph drifts, R1c scores the snapshot, not a fresh resolve; a new `prepare` means a new
adjudication version.

## Part C: measurements (conductor, read-only, counts only)

C1. Folding overlap from the B1 `replay.json` and fresh `node_meta.json`, per case: plain
   `resolve-asker` items; how many map (self or `thread_root`) to a root `resolve-lead`
   returned; how many plain items share a root with another plain item; lead roots absent
   from plain. The remainder is reported as "not explained by folding", with no attribution
   to ACL, epic, deletion or budget.
C2. `authored_by_bot`: (a) from `replay.json` items, `unknown/human/bot` among non-seed lead
   roots; (b) hub DB read-only (`PGOPTIONS="-c default_transaction_read_only=on"`), counts:
   Slack nodes with `author_person_id IS NULL` over all Slack nodes; Slack nodes whose person
   has `slack_user_id IS NULL`. Check column names in `information_schema.columns` first.
   Report missing author linkage and bot classification of known people separately. bd issue
   in agent-mem: P2 if (a) unknown > 30 %, else P3.

## Files expected to change

Part A: `cmd/agent-mem/eval_common.go`, `eval_prepare.go`, `eval_relevance.go`, `eval.go`,
new `cmd/agent-mem/eval_relation.go`, tests under `cmd/agent-mem/`, new
`internal/redact/pseudonym.go` + `pseudonym_test.go` (shared regexes may move within
`internal/redact`), the eval doc.
Part B: `testdata/eval/graph_context/replay.json`, `node_meta.json`, new
`testdata/eval/graph_context/relation_labels.json`. Nothing else.

## Acceptance

1. Part A gates green (Gates above), including the rg check.
2. `go test ./cmd/agent-mem/ -run TestEvalDryRunLeadVariant` passes (synthetic fixture; no
   real query text printed).
3. After B1, from the worktree with the B1 shell env:
   `$PRIV/agent-mem eval relevance --quiet --base-url http://localhost:34567 >
   $PRIV/after.txt`: `resolve-lead` shows `warmup_ok 20/20` and `runs_ok 200/200`. Failure
   counts of the old variants are compared with `$PRIV/before.txt`; a difference is
   reported as graph drift, not as a code result.
4. `relation-check` passes on the merged file; its counts go into Results.
5. Sheet scan (local, counts only): over title and summary lines of `$PRIV/sheets/*.md`,
   zero matches for `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}`, `<@[UW]`, `\b\d{8,}\b`.
   This is a narrow audit; the A5/A8 tests and the owner decision below are the boundary.
6. `git -C <wt> diff --name-only $PART_B_BASE HEAD` on the Part B branch lists exactly the
   3 Part B files; `git -C <wt> status --porcelain --ignored -- testdata docs` is empty.
7. `git -C /Users/enzo/go/src/github.com/agent-mem status --porcelain` and `rev-parse HEAD`
   are identical before B1 and after the round.
8. Part C numbers and the bd issue id in Results.

## Verify (conductor)

- After Part A merge: acceptance 1-2 from the worktree.
- After Part B merge: acceptance 3-7.

## Owner decisions on the approval page

- Approving this plan authorizes sending the masked sheets to pi (OpenAI backend) and to an
  Opus subagent. Residual exposure: names in prose that are not mentions or authors,
  partner and merchant names, refs without a digit or outside the A5 grammar.
- `different_case` labels are adjudicated by two independent model reads; disagreements
  become `insufficient` and are excluded unless Enzo answers B5.

## Results

(filled after Part B and Part C)
